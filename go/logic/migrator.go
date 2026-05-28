/*
   Copyright 2025 GitHub Inc.
	 See https://github.com/github/gh-ost/blob/master/LICENSE
*/

package logic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"os"

	"github.com/github/gh-ost/go/base"
	"github.com/github/gh-ost/go/binlog"
	"github.com/github/gh-ost/go/mysql"
	"github.com/github/gh-ost/go/sql"
)

var (
	ErrMigratorUnsupportedRenameAlter = errors.New("alter statement seems to RENAME the table. This is not supported, and you should run your RENAME outside gh-ost")
	ErrMigrationNotAllowedOnMaster    = errors.New("it seems like this migration attempt to run directly on master. Preferably it would be executed on a replica (this reduces load from the master). To proceed please provide --allow-on-master")
	RetrySleepFn                      = time.Sleep
	checkpointTimeout                 = 2 * time.Second
)

type ChangelogState string

const (
	AllEventsUpToLockProcessed ChangelogState = "AllEventsUpToLockProcessed"
	GhostTableMigrated         ChangelogState = "GhostTableMigrated"
	Migrated                   ChangelogState = "Migrated"
	ReadMigrationRangeValues   ChangelogState = "ReadMigrationRangeValues"
)

func ReadChangelogState(s string) ChangelogState {
	return ChangelogState(strings.Split(s, ":")[0])
}

type tableWriteFunc func() error

type lockProcessedStruct struct {
	state  string
	coords mysql.BinlogCoordinates
}
type PrintStatusRule int

const (
	NoPrintStatusRule           PrintStatusRule = iota
	HeuristicPrintStatusRule                    = iota
	ForcePrintStatusRule                        = iota
	ForcePrintStatusOnlyRule                    = iota
	ForcePrintStatusAndHintRule                 = iota
)

// Migrator is the main schema migration flow manager.
type Migrator struct {
	appVersion       string
	parser           *sql.AlterTableParser
	inspector        *Inspector
	applier          *Applier
	server           *Server
	throttler        *Throttler
	hooksExecutor    base.Hooks
	migrationContext *base.MigrationContext

	firstThrottlingCollected   chan bool
	ghostTableMigrated         chan bool
	rowCopyComplete            chan error
	allEventsUpToLockProcessed chan *lockProcessedStruct
	lastLockProcessed          *lockProcessedStruct

	rowCopyCompleteFlag int64
	// copyRowsQueue should not be buffered; if buffered some non-damaging but
	//  excessive work happens at the end of the iteration as new copy-jobs arrive before realizing the copy is complete
	copyRowsQueue chan tableWriteFunc

	finishedMigrating int64
	trxCoordinator    *Coordinator
}

func NewMigrator(context *base.MigrationContext, appVersion string) *Migrator {
	hooks := context.Hooks
	if hooks == nil {
		hooks = NewHooksExecutor(context)
	}
	migrator := &Migrator{
		appVersion:                 appVersion,
		hooksExecutor:              hooks,
		migrationContext:           context,
		parser:                     sql.NewAlterTableParser(),
		ghostTableMigrated:         make(chan bool, 1),
		firstThrottlingCollected:   make(chan bool, 3),
		rowCopyComplete:            make(chan error),
		allEventsUpToLockProcessed: make(chan *lockProcessedStruct, 1),

		copyRowsQueue:     make(chan tableWriteFunc),
		finishedMigrating: 0,
	}
	return migrator
}

// sleepWhileTrue sleeps indefinitely until the given function returns 'false'
// (or fails with error)
func (mig *Migrator) sleepWhileTrue(operation func() (bool, error)) error {
	for {
		// Check for abort before continuing
		if err := mig.checkAbort(); err != nil {
			return err
		}
		shouldSleep, err := operation()
		if err != nil {
			return err
		}
		if !shouldSleep {
			return nil
		}
		time.Sleep(time.Second)
	}
}

func (mig *Migrator) retryBatchCopyWithHooks(operation func() error, notFatalHint ...bool) (err error) {
	wrappedOperation := func() error {
		if err := operation(); err != nil {
			mig.hooksExecutor.OnBatchCopyRetry(err.Error())
			return err
		}
		return nil
	}

	return mig.retryOperation(wrappedOperation, notFatalHint...)
}

// retryOperation attempts up to `count` attempts at running given function,
// exiting as soon as it returns with non-error.
func (mig *Migrator) retryOperation(operation func() error, notFatalHint ...bool) (err error) {
	maxRetries := int(mig.migrationContext.MaxRetries())
	for i := 0; i < maxRetries; i++ {
		if i != 0 {
			// sleep after previous iteration
			RetrySleepFn(1 * time.Second)
		}
		// Check for abort/context cancellation before each retry
		if abortErr := mig.checkAbort(); abortErr != nil {
			return abortErr
		}
		err = operation()
		if err == nil {
			return nil
		}
		// Check if this is an unrecoverable error (data consistency issues won't resolve on retry)
		if strings.Contains(err.Error(), "warnings detected") {
			if len(notFatalHint) == 0 {
				_ = base.SendWithContext(mig.migrationContext.GetContext(), mig.migrationContext.PanicAbort, err)
			}
			return err
		}
		// there's an error. Let's try again.
	}
	if len(notFatalHint) == 0 {
		// Use helper to prevent deadlock if listenOnPanicAbort already exited
		_ = base.SendWithContext(mig.migrationContext.GetContext(), mig.migrationContext.PanicAbort, err)
	}
	return err
}

// `retryOperationWithExponentialBackoff` attempts running given function, waiting 2^(n-1)
// seconds between each attempt, where `n` is the running number of attempts. Exits
// as soon as the function returns with non-error, or as soon as `MaxRetries`
// attempts are reached. Wait intervals between attempts obey a maximum of
// `ExponentialBackoffMaxInterval`.
func (mig *Migrator) retryOperationWithExponentialBackoff(operation func() error, notFatalHint ...bool) (err error) {
	maxRetries := int(mig.migrationContext.MaxRetries())
	maxInterval := mig.migrationContext.ExponentialBackoffMaxInterval
	for i := 0; i < maxRetries; i++ {
		interval := math.Min(
			float64(maxInterval),
			math.Max(1, math.Exp2(float64(i-1))),
		)

		if i != 0 {
			RetrySleepFn(time.Duration(interval) * time.Second)
		}
		// Check for abort/context cancellation before each retry
		if abortErr := mig.checkAbort(); abortErr != nil {
			return abortErr
		}
		err = operation()
		if err == nil {
			return nil
		}
		// Check if this is an unrecoverable error (data consistency issues won't resolve on retry)
		if strings.Contains(err.Error(), "warnings detected") {
			if len(notFatalHint) == 0 {
				_ = base.SendWithContext(mig.migrationContext.GetContext(), mig.migrationContext.PanicAbort, err)
			}
			return err
		}
	}
	if len(notFatalHint) == 0 {
		// Use helper to prevent deadlock if listenOnPanicAbort already exited
		_ = base.SendWithContext(mig.migrationContext.GetContext(), mig.migrationContext.PanicAbort, err)
	}
	return err
}

// consumeRowCopyComplete blocks on the rowCopyComplete channel once, and then
// consumes and drops any further incoming events that may be left hanging.
func (mig *Migrator) consumeRowCopyComplete() {
	if err := <-mig.rowCopyComplete; err != nil {
		// Abort synchronously to ensure checkAbort() sees the error immediately
		mig.abort(err)
		// Don't mark row copy as complete if there was an error
		return
	}
	atomic.StoreInt64(&mig.rowCopyCompleteFlag, 1)
	mig.migrationContext.MarkRowCopyEndTime()
	go func() {
		for err := range mig.rowCopyComplete {
			if err != nil {
				// Abort synchronously to ensure the error is stored immediately
				mig.abort(err)
				return
			}
		}
	}()
}

func (mig *Migrator) canStopStreaming() bool {
	return atomic.LoadInt64(&mig.migrationContext.CutOverCompleteFlag) != 0
}

// onChangelogEvent is called when a binlog event operation on the changelog table is intercepted.
func (mig *Migrator) onChangelogEvent(dmlEvent *binlog.BinlogDMLEvent) (err error) {
	// Hey, I created the changelog table, I know the type of columns it has!
	switch hint := dmlEvent.NewColumnValues.StringColumn(2); hint {
	case "state":
		return mig.onChangelogStateEvent(dmlEvent)
	case "heartbeat":
		return mig.onChangelogHeartbeatEvent(dmlEvent)
	default:
		return nil
	}
}

func (mig *Migrator) onChangelogStateEvent(dmlEvent *binlog.BinlogDMLEvent) (err error) {
	changelogStateString := dmlEvent.NewColumnValues.StringColumn(3)
	changelogState := ReadChangelogState(changelogStateString)
	mig.migrationContext.Log.Infof("Intercepted changelog state %s", changelogState)
	switch changelogState {
	case Migrated, ReadMigrationRangeValues:
		// no-op event
	case GhostTableMigrated:
		// Use helper to prevent deadlock if migration aborts before receiver is ready
		_ = base.SendWithContext(mig.migrationContext.GetContext(), mig.ghostTableMigrated, true)
	case AllEventsUpToLockProcessed:
		// at this point we know all events up to lock have been read from the streamer,
		// because the streamer works sequentially. So those events are either already handled,
		// or are being processed by the coordinator.
		// So as not to create a potential deadlock, we send this asynchronously.
		go func() {
			_ = base.SendWithContext(mig.migrationContext.GetContext(), mig.allEventsUpToLockProcessed, &lockProcessedStruct{
				state:  changelogStateString,
				coords: mig.trxCoordinator.binlogReader.GetCurrentBinlogCoordinates(),
			})
		}()
	default:
		return fmt.Errorf("unknown changelog state: %+v", changelogState)
	}
	mig.migrationContext.Log.Infof("Handled changelog state %s", changelogState)
	return nil
}

func (mig *Migrator) onChangelogHeartbeatEvent(dmlEvent *binlog.BinlogDMLEvent) (err error) {
	changelogHeartbeatString := dmlEvent.NewColumnValues.StringColumn(3)

	heartbeatTime, err := time.Parse(time.RFC3339Nano, changelogHeartbeatString)
	if err != nil {
		return mig.migrationContext.Log.Errore(err)
	} else {
		mig.migrationContext.SetLastHeartbeatOnChangelogTime(heartbeatTime)
		mig.applier.CurrentCoordinatesMutex.Lock()
		mig.applier.CurrentCoordinates = mig.trxCoordinator.binlogReader.GetCurrentBinlogCoordinates()
		mig.applier.CurrentCoordinatesMutex.Unlock()
		return nil
	}
}

// abort stores the error, cancels the context, and logs the abort.
// This is the common abort logic used by both listenOnPanicAbort and
// consumeRowCopyComplete to ensure consistent error handling.
func (mig *Migrator) abort(err error) {
	// Store the error for Migrate() to return
	mig.migrationContext.SetAbortError(err)

	// Cancel the context to signal all goroutines to stop
	mig.migrationContext.CancelContext()

	// Log the error (but don't panic or exit)
	mig.migrationContext.Log.Errorf("Migration aborted: %v", err)
}

// listenOnPanicAbort listens for fatal errors and initiates graceful shutdown
func (mig *Migrator) listenOnPanicAbort() {
	err := <-mig.migrationContext.PanicAbort
	mig.abort(err)
}

// validateAlterStatement validates the `alter` statement meets criteria.
// At this time this means:
// - column renames are approved
// - no table rename allowed
func (mig *Migrator) validateAlterStatement() (err error) {
	if mig.parser.IsRenameTable() {
		return ErrMigratorUnsupportedRenameAlter
	}
	if mig.parser.HasNonTrivialRenames() && !mig.migrationContext.SkipRenamedColumns {
		mig.migrationContext.ColumnRenameMap = mig.parser.GetNonTrivialRenames()
		if !mig.migrationContext.ApproveRenamedColumns {
			return fmt.Errorf("gh-ost believes the ALTER statement renames columns, as follows: %v; as precaution, you are asked to confirm gh-ost is correct, and provide with `--approve-renamed-columns`, and we're all happy. Or you can skip renamed columns via `--skip-renamed-columns`, in which case column data may be lost", mig.parser.GetNonTrivialRenames())
		}
		mig.migrationContext.Log.Infof("Alter statement has column(s) renamed. gh-ost finds the following renames: %v; --approve-renamed-columns is given and so migration proceeds.", mig.parser.GetNonTrivialRenames())
	}
	mig.migrationContext.DroppedColumnsMap = mig.parser.DroppedColumnsMap()
	return nil
}

func (mig *Migrator) countTableRows() (err error) {
	if !mig.migrationContext.CountTableRows {
		// Not counting; we stay with an estimate
		return nil
	}
	if mig.migrationContext.Noop {
		mig.migrationContext.Log.Debugf("Noop operation; not really counting table rows")
		return nil
	}

	countRowsFunc := func(ctx context.Context) error {
		if err := mig.inspector.CountTableRows(ctx); err != nil {
			return err
		}
		if err := mig.hooksExecutor.OnRowCountComplete(); err != nil {
			return err
		}
		return nil
	}

	if mig.migrationContext.ConcurrentCountTableRows {
		// store a cancel func so we can stop this query before a cut over
		rowCountContext, rowCountCancel := context.WithCancel(context.Background())
		mig.migrationContext.SetCountTableRowsCancelFunc(rowCountCancel)

		mig.migrationContext.Log.Infof("As instructed, counting rows in the background; meanwhile I will use an estimated count, and will update it later on")
		go countRowsFunc(rowCountContext)

		// and we ignore errors, because this turns to be a background job
		return nil
	}
	return countRowsFunc(context.Background())
}

func (mig *Migrator) createFlagFiles() (err error) {
	if mig.migrationContext.PostponeCutOverFlagFile != "" {
		if !base.FileExists(mig.migrationContext.PostponeCutOverFlagFile) {
			if err := base.TouchFile(mig.migrationContext.PostponeCutOverFlagFile); err != nil {
				return mig.migrationContext.Log.Errorf("--postpone-cut-over-flag-file indicated by gh-ost is unable to create said file: %s", err.Error())
			}
			mig.migrationContext.Log.Infof("Created postpone-cut-over-flag-file: %s", mig.migrationContext.PostponeCutOverFlagFile)
		}
	}
	return nil
}

// checkAbort returns abort error if migration was aborted
func (mig *Migrator) checkAbort() error {
	if abortErr := mig.migrationContext.GetAbortError(); abortErr != nil {
		return abortErr
	}

	ctx := mig.migrationContext.GetContext()
	if ctx != nil {
		select {
		case <-ctx.Done():
			// Context cancelled but no abort error stored yet
			if abortErr := mig.migrationContext.GetAbortError(); abortErr != nil {
				return abortErr
			}
			return ctx.Err()
		default:
			// Not cancelled
		}
	}
	return nil
}

// Migrate executes the complete migration logic. This is *the* major gh-ost function.
func (mig *Migrator) Migrate() (err error) {
	mig.migrationContext.Log.Infof("Migrating %s.%s", sql.EscapeName(mig.migrationContext.DatabaseName), sql.EscapeName(mig.migrationContext.OriginalTableName))
	mig.migrationContext.StartTime = time.Now()

	// Ensure context is cancelled on exit (cleanup)
	defer mig.migrationContext.CancelContext()

	if mig.migrationContext.Hostname, err = os.Hostname(); err != nil {
		return err
	}

	go mig.listenOnPanicAbort()

	if err := mig.hooksExecutor.OnStartup(); err != nil {
		return err
	}
	if err := mig.parser.ParseAlterStatement(mig.migrationContext.AlterStatement); err != nil {
		return err
	}
	if err := mig.validateAlterStatement(); err != nil {
		return err
	}

	// After this point, we'll need to teardown anything that's been started
	//   so we don't leave things hanging around
	defer mig.teardown()

	if err := mig.initiateInspector(); err != nil {
		return err
	}

	mig.trxCoordinator = NewCoordinator(mig.migrationContext, mig.applier, mig.throttler, mig.onChangelogEvent)

	if err := mig.checkAbort(); err != nil {
		return err
	}
	// If we are resuming, we will initiateStreaming later when we know
	// the binlog coordinates to resume streaming from.
	// If not resuming, the streamer must be initiated before the applier,
	// so that the "GhostTableMigrated" event gets processed.
	if !mig.migrationContext.Resume {
		if err := mig.initiateStreaming(); err != nil {
			return err
		}
		if err := mig.checkAbort(); err != nil {
			return err
		}
	}
	if err := mig.initiateApplier(); err != nil {
		return err
	}
	if err := mig.checkAbort(); err != nil {
		return err
	}

	mig.trxCoordinator.applier = mig.applier

	if err := mig.createFlagFiles(); err != nil {
		return err
	}
	// In MySQL 8.0 (and possibly earlier) some DDL statements can be applied instantly.
	// Attempt to do this if AttemptInstantDDL is set.
	if mig.migrationContext.AttemptInstantDDL {
		if mig.migrationContext.Noop {
			mig.migrationContext.Log.Debugf("Noop operation; not really attempting instant DDL")
		} else {
			mig.migrationContext.Log.Infof("Attempting to execute alter with ALGORITHM=INSTANT")
			if err := mig.applier.AttemptInstantDDL(); err == nil {
				if err := mig.finalCleanup(); err != nil {
					return nil
				}
				if err := mig.hooksExecutor.OnSuccess(true); err != nil {
					return err
				}
				mig.migrationContext.Log.Infof("Success! table %s.%s migrated instantly", sql.EscapeName(mig.migrationContext.DatabaseName), sql.EscapeName(mig.migrationContext.OriginalTableName))
				return nil
			} else {
				mig.migrationContext.Log.Infof("ALGORITHM=INSTANT not supported for this operation, proceeding with original algorithm: %s", err)
			}
		}
	}

	mig.migrationContext.Log.Infof("starting %d applier workers", mig.migrationContext.NumWorkers)
	mig.trxCoordinator.InitializeWorkers(mig.migrationContext.NumWorkers)

	initialLag, _ := mig.inspector.getReplicationLag()
	if !mig.migrationContext.Resume {
		mig.migrationContext.Log.Infof("Waiting for ghost table to be migrated. Current lag is %+v", initialLag)

	waitForGhostTable:
		for {
			select {
			case <-mig.ghostTableMigrated:
				break waitForGhostTable
			default:
				dmlEvent, err := mig.trxCoordinator.ProcessEventsUntilNextChangelogEvent()
				if err != nil {
					return err
				}

				mig.onChangelogEvent(dmlEvent)
			}
		}

		mig.migrationContext.Log.Debugf("ghost table migrated")
	}
	// Yay! We now know the Ghost and Changelog tables are good to examine!
	// When running on replica, this means the replica has those tables. When running
	// on master this is always true, of course, and yet it also implies this knowledge
	// is in the binlogs.
	if err := mig.inspector.inspectOriginalAndGhostTables(); err != nil {
		return err
	}

	// We can prepare some of the queries on the applier
	if err := mig.applier.prepareQueries(); err != nil {
		return err
	}

	// inspectOriginalAndGhostTables must be called before creating checkpoint table.
	if mig.migrationContext.Checkpoint && !mig.migrationContext.Resume {
		if err := mig.applier.CreateCheckpointTable(); err != nil {
			mig.migrationContext.Log.Errorf("Unable to create checkpoint table, see further error details.")
		}
	}

	if mig.migrationContext.Resume {
		lastCheckpoint, err := mig.applier.ReadLastCheckpoint()
		if err != nil {
			return mig.migrationContext.Log.Errorf("no checkpoint found, unable to resume: %+v", err)
		}
		mig.migrationContext.Log.Infof("Resuming from checkpoint coords=%+v range_min=%+v range_max=%+v iteration=%d",
			lastCheckpoint.LastTrxCoords, lastCheckpoint.IterationRangeMin.String(), lastCheckpoint.IterationRangeMax.String(), lastCheckpoint.Iteration)

		mig.migrationContext.MigrationIterationRangeMinValues = lastCheckpoint.IterationRangeMin
		mig.migrationContext.MigrationIterationRangeMaxValues = lastCheckpoint.IterationRangeMax
		mig.migrationContext.Iteration = lastCheckpoint.Iteration
		mig.migrationContext.TotalRowsCopied = lastCheckpoint.RowsCopied
		mig.migrationContext.TotalDMLEventsApplied = lastCheckpoint.DMLApplied
		mig.migrationContext.InitialStreamerCoords = lastCheckpoint.LastTrxCoords
		if err := mig.initiateStreaming(); err != nil {
			return err
		}
	}

	// Validation complete! We're good to execute this migration
	if err := mig.hooksExecutor.OnValidated(); err != nil {
		return err
	}

	if err := mig.initiateServer(); err != nil {
		return err
	}
	defer mig.server.RemoveSocketFile()

	if err := mig.countTableRows(); err != nil {
		return err
	}

	if err := mig.applier.ReadMigrationRangeValues(); err != nil {
		return err
	}

	mig.initiateThrottler()

	if err := mig.hooksExecutor.OnBeforeRowCopy(); err != nil {
		return err
	}
	go func() {
		if err := mig.executeWriteFuncs(); err != nil {
			// Send error to PanicAbort to trigger abort
			_ = base.SendWithContext(mig.migrationContext.GetContext(), mig.migrationContext.PanicAbort, err)
		}
	}()
	go mig.iterateChunks()
	mig.migrationContext.MarkRowCopyStartTime()
	go mig.initiateStatus()
	if mig.migrationContext.Checkpoint {
		go mig.checkpointLoop()
	}

	mig.migrationContext.Log.Debugf("Operating until row copy is complete")
	mig.consumeRowCopyComplete()
	mig.migrationContext.Log.Infof("Row copy complete")
	// Check if row copy was aborted due to error
	if err := mig.checkAbort(); err != nil {
		return err
	}
	if err := mig.hooksExecutor.OnRowCopyComplete(); err != nil {
		return err
	}
	mig.printStatus(ForcePrintStatusRule)
	mig.printWorkerStats()

	if mig.migrationContext.IsCountingTableRows() {
		mig.migrationContext.Log.Info("stopping query for exact row count, because that can accidentally lock out the cut over")
		mig.migrationContext.CancelTableRowsCount()
	}
	if err := mig.hooksExecutor.OnBeforeCutOver(); err != nil {
		return err
	}
	var retrier func(func() error, ...bool) error
	if mig.migrationContext.CutOverExponentialBackoff {
		retrier = mig.retryOperationWithExponentialBackoff
	} else {
		retrier = mig.retryOperation
	}
	if err := retrier(mig.cutOver); err != nil {
		return err
	}
	atomic.StoreInt64(&mig.migrationContext.CutOverCompleteFlag, 1)

	if mig.migrationContext.Checkpoint && !mig.migrationContext.Noop {
		cutoverChk, err := mig.CheckpointAfterCutOver()
		if err != nil {
			mig.migrationContext.Log.Warningf("failed to checkpoint after cutover: %+v", err)
		} else {
			mig.migrationContext.Log.Infof("checkpoint success after cutover at coords=%+v", cutoverChk.LastTrxCoords.DisplayString())
		}
	}

	if err := mig.finalCleanup(); err != nil {
		return nil
	}
	if err := mig.hooksExecutor.OnSuccess(false); err != nil {
		return err
	}
	mig.migrationContext.Log.Infof("Done migrating %s.%s", sql.EscapeName(mig.migrationContext.DatabaseName), sql.EscapeName(mig.migrationContext.OriginalTableName))
	// Final check for abort before declaring success
	if err := mig.checkAbort(); err != nil {
		return err
	}
	return nil
}

// Revert reverts a migration that previously completed by applying all DML events that happened
// after the original cutover, then doing another cutover to swap the tables back.
// The steps are similar to Migrate(), but without row copying.
func (mig *Migrator) Revert() error {
	mig.migrationContext.Log.Infof("Reverting %s.%s from %s.%s",
		sql.EscapeName(mig.migrationContext.DatabaseName), sql.EscapeName(mig.migrationContext.OriginalTableName),
		sql.EscapeName(mig.migrationContext.DatabaseName), sql.EscapeName(mig.migrationContext.OldTableName))
	mig.migrationContext.StartTime = time.Now()

	// Ensure context is cancelled on exit (cleanup)
	defer mig.migrationContext.CancelContext()

	var err error
	if mig.migrationContext.Hostname, err = os.Hostname(); err != nil {
		return err
	}

	go mig.listenOnPanicAbort()

	if err := mig.hooksExecutor.OnStartup(); err != nil {
		return err
	}
	if err := mig.validateAlterStatement(); err != nil {
		return err
	}
	defer mig.teardown()

	if err := mig.initiateInspector(); err != nil {
		return err
	}
	if err := mig.checkAbort(); err != nil {
		return err
	}
	if err := mig.initiateApplier(); err != nil {
		return err
	}
	if err := mig.checkAbort(); err != nil {
		return err
	}
	if err := mig.createFlagFiles(); err != nil {
		return err
	}
	if err := mig.inspector.inspectOriginalAndGhostTables(); err != nil {
		return err
	}
	if err := mig.applier.prepareQueries(); err != nil {
		return err
	}

	lastCheckpoint, err := mig.applier.ReadLastCheckpoint()
	if err != nil {
		return mig.migrationContext.Log.Errorf("no checkpoint found, unable to revert: %+v", err)
	}
	if !lastCheckpoint.IsCutover {
		return mig.migrationContext.Log.Errorf("Last checkpoint is not after cutover, unable to revert: coords=%+v time=%+v", lastCheckpoint.LastTrxCoords, lastCheckpoint.Timestamp)
	}
	mig.migrationContext.InitialStreamerCoords = lastCheckpoint.LastTrxCoords
	mig.migrationContext.TotalRowsCopied = lastCheckpoint.RowsCopied
	mig.migrationContext.MigrationIterationRangeMinValues = lastCheckpoint.IterationRangeMin
	mig.migrationContext.MigrationIterationRangeMaxValues = lastCheckpoint.IterationRangeMax
	if err := mig.initiateStreaming(); err != nil {
		return err
	}
	if err := mig.checkAbort(); err != nil {
		return err
	}
	if err := mig.hooksExecutor.OnValidated(); err != nil {
		return err
	}
	if err := mig.initiateServer(); err != nil {
		return err
	}
	defer mig.server.RemoveSocketFile()

	mig.initiateThrottler()
	go mig.initiateStatus()
	go func() {
		if err := mig.executeWriteFuncs(); err != nil {
			// Send error to PanicAbort to trigger abort
			_ = base.SendWithContext(mig.migrationContext.GetContext(), mig.migrationContext.PanicAbort, err)
		}
	}()

	mig.printStatus(ForcePrintStatusRule)
	var retrier func(func() error, ...bool) error
	if mig.migrationContext.CutOverExponentialBackoff {
		retrier = mig.retryOperationWithExponentialBackoff
	} else {
		retrier = mig.retryOperation
	}
	if err := mig.hooksExecutor.OnBeforeCutOver(); err != nil {
		return err
	}
	if err := retrier(mig.cutOver); err != nil {
		return err
	}
	atomic.StoreInt64(&mig.migrationContext.CutOverCompleteFlag, 1)
	if err := mig.finalCleanup(); err != nil {
		return nil
	}
	if err := mig.hooksExecutor.OnSuccess(false); err != nil {
		return err
	}
	mig.migrationContext.Log.Infof("Done reverting %s.%s", sql.EscapeName(mig.migrationContext.DatabaseName), sql.EscapeName(mig.migrationContext.OriginalTableName))
	return nil
}

// ExecOnFailureHook executes the onFailure hook, and this method is provided as the only external
// hook access point
func (mig *Migrator) ExecOnFailureHook() (err error) {
	return mig.hooksExecutor.OnFailure()
}

func (mig *Migrator) handleCutOverResult(cutOverError error) (err error) {
	if mig.migrationContext.TestOnReplica {
		// We're merely testing, we don't want to keep this state. Rollback the renames as possible
		mig.applier.RenameTablesRollback()
	}
	if cutOverError == nil {
		return nil
	}
	// Only on error:

	if mig.migrationContext.TestOnReplica {
		// With `--test-on-replica` we stop replication thread, and then proceed to use
		// the same cut-over phase as the master would use. That means we take locks
		// and swap the tables.
		// The difference is that we will later swap the tables back.
		if err := mig.hooksExecutor.OnStartReplication(); err != nil {
			return mig.migrationContext.Log.Errore(err)
		}
		if mig.migrationContext.TestOnReplicaSkipReplicaStop {
			mig.migrationContext.Log.Warningf("--test-on-replica-skip-replica-stop enabled, we are not starting replication.")
		} else {
			mig.migrationContext.Log.Debugf("testing on replica. Starting replication IO thread after cut-over failure")
			if err := mig.retryOperation(mig.applier.StartReplication); err != nil {
				return mig.migrationContext.Log.Errore(err)
			}
		}
	}
	return nil
}

// cutOver performs the final step of migration, based on migration
// type (on replica? atomic? safe?)
func (mig *Migrator) cutOver() (err error) {
	if mig.migrationContext.Noop {
		mig.migrationContext.Log.Debugf("Noop operation; not really swapping tables")
		return nil
	}
	mig.migrationContext.MarkPointOfInterest()
	mig.throttler.throttle(func() {
		mig.migrationContext.Log.Debugf("throttling before swapping tables")
	})

	mig.migrationContext.MarkPointOfInterest()
	mig.migrationContext.Log.Debugf("checking for cut-over postpone")
	if err := mig.sleepWhileTrue(
		func() (bool, error) {
			heartbeatLag := mig.migrationContext.TimeSinceLastHeartbeatOnChangelog()
			maxLagMillisecondsThrottle := time.Duration(atomic.LoadInt64(&mig.migrationContext.MaxLagMillisecondsThrottleThreshold)) * time.Millisecond
			cutOverLockTimeout := time.Duration(mig.migrationContext.CutOverLockTimeoutSeconds) * time.Second
			if heartbeatLag > maxLagMillisecondsThrottle || heartbeatLag > cutOverLockTimeout {
				mig.migrationContext.Log.Debugf("current HeartbeatLag (%.2fs) is too high, it needs to be less than both --max-lag-millis (%.2fs) and --cut-over-lock-timeout-seconds (%.2fs) to continue", heartbeatLag.Seconds(), maxLagMillisecondsThrottle.Seconds(), cutOverLockTimeout.Seconds())
				return true, nil
			}
			if mig.migrationContext.PostponeCutOverFlagFile == "" {
				return false, nil
			}
			if atomic.LoadInt64(&mig.migrationContext.UserCommandedUnpostponeFlag) > 0 {
				atomic.StoreInt64(&mig.migrationContext.UserCommandedUnpostponeFlag, 0)
				return false, nil
			}
			if base.FileExists(mig.migrationContext.PostponeCutOverFlagFile) {
				// Postpone file defined and exists!
				if atomic.LoadInt64(&mig.migrationContext.IsPostponingCutOver) == 0 {
					if err := mig.hooksExecutor.OnBeginPostponed(); err != nil {
						return true, err
					}
				}
				atomic.StoreInt64(&mig.migrationContext.IsPostponingCutOver, 1)
				return true, nil
			}
			return false, nil
		},
	); err != nil {
		return err
	}
	atomic.StoreInt64(&mig.migrationContext.IsPostponingCutOver, 0)
	mig.migrationContext.MarkPointOfInterest()
	mig.migrationContext.Log.Debugf("checking for cut-over postpone: complete")

	if mig.migrationContext.TestOnReplica {
		// With `--test-on-replica` we stop replication thread, and then proceed to use
		// the same cut-over phase as the master would use. That means we take locks
		// and swap the tables.
		// The difference is that we will later swap the tables back.
		if err := mig.hooksExecutor.OnStopReplication(); err != nil {
			return err
		}
		if mig.migrationContext.TestOnReplicaSkipReplicaStop {
			mig.migrationContext.Log.Warningf("--test-on-replica-skip-replica-stop enabled, we are not stopping replication.")
		} else {
			mig.migrationContext.Log.Debugf("testing on replica. Stopping replication IO thread")
			if err := mig.retryOperation(mig.applier.StopReplication); err != nil {
				return err
			}
		}
	}

	switch mig.migrationContext.CutOverType {
	case base.CutOverAtomic:
		// Atomic solution: we use low timeout and multiple attempts. But for
		// each failed attempt, we throttle until replication lag is back to normal
		err = mig.atomicCutOver()
	case base.CutOverTwoStep:
		err = mig.cutOverTwoStep()
	default:
		return mig.migrationContext.Log.Fatalf("Unknown cut-over type: %d; should never get here!", mig.migrationContext.CutOverType)
	}
	mig.handleCutOverResult(err)
	return err
}

// Inject the "AllEventsUpToLockProcessed" state hint, wait for it to appear in the binary logs,
// make sure the queue is drained.
func (mig *Migrator) waitForEventsUpToLock() error {
	timeout := time.NewTimer(time.Second * time.Duration(mig.migrationContext.CutOverLockTimeoutSeconds))

	mig.migrationContext.MarkPointOfInterest()
	waitForEventsUpToLockStartTime := time.Now()

	allEventsUpToLockProcessedChallenge := fmt.Sprintf("%s:%d", string(AllEventsUpToLockProcessed), waitForEventsUpToLockStartTime.UnixNano())
	mig.migrationContext.Log.Infof("Writing changelog state: %+v", allEventsUpToLockProcessedChallenge)
	if _, err := mig.applier.WriteChangelogState(allEventsUpToLockProcessedChallenge); err != nil {
		return err
	}
	mig.migrationContext.Log.Infof("Waiting for events up to lock")
	atomic.StoreInt64(&mig.migrationContext.AllEventsUpToLockProcessedInjectedFlag, 1)
	var lockProcessed *lockProcessedStruct
	for found := false; !found; {
		select {
		case <-timeout.C:
			{
				return mig.migrationContext.Log.Errorf("Timeout while waiting for events up to lock")
			}
		case lockProcessed = <-mig.allEventsUpToLockProcessed:
			{
				if lockProcessed.state == allEventsUpToLockProcessedChallenge {
					mig.migrationContext.Log.Infof("Waiting for events up to lock: got %s", lockProcessed.state)
					found = true
					mig.lastLockProcessed = lockProcessed
				} else {
					mig.migrationContext.Log.Infof("Waiting for events up to lock: skipping %s", lockProcessed.state)
				}
			}
		}
	}
	waitForEventsUpToLockDuration := time.Since(waitForEventsUpToLockStartTime)

	mig.migrationContext.Log.Infof("Done waiting for events up to lock; duration=%+v", waitForEventsUpToLockDuration)
	mig.printStatus(ForcePrintStatusAndHintRule)

	return nil
}

// cutOverTwoStep will lock down the original table, execute
// what's left of last DML entries, and **non-atomically** swap original->old, then new->original.
// There is a point in time where the "original" table does not exist and queries are non-blocked
// and failing.
func (mig *Migrator) cutOverTwoStep() (err error) {
	atomic.StoreInt64(&mig.migrationContext.InCutOverCriticalSectionFlag, 1)
	defer atomic.StoreInt64(&mig.migrationContext.InCutOverCriticalSectionFlag, 0)
	atomic.StoreInt64(&mig.migrationContext.AllEventsUpToLockProcessedInjectedFlag, 0)

	if err := mig.retryOperation(mig.applier.LockOriginalTable); err != nil {
		return err
	}

	if err := mig.retryOperation(mig.waitForEventsUpToLock); err != nil {
		return err
	}
	// If we need to create triggers we need to do it here (only create part)
	if mig.migrationContext.IncludeTriggers && len(mig.migrationContext.Triggers) > 0 {
		if err := mig.retryOperation(mig.applier.CreateTriggersOnGhost); err != nil {
			return err
		}
	}
	if err := mig.retryOperation(mig.applier.SwapTablesQuickAndBumpy); err != nil {
		return err
	}
	if err := mig.retryOperation(mig.applier.UnlockTables); err != nil {
		return err
	}

	lockAndRenameDuration := mig.migrationContext.RenameTablesEndTime.Sub(mig.migrationContext.LockTablesStartTime)
	renameDuration := mig.migrationContext.RenameTablesEndTime.Sub(mig.migrationContext.RenameTablesStartTime)
	mig.migrationContext.Log.Debugf("Lock & rename duration: %s (rename only: %s). During this time, queries on %s were locked or failing", lockAndRenameDuration, renameDuration, sql.EscapeName(mig.migrationContext.OriginalTableName))
	return nil
}

// atomicCutOver
func (mig *Migrator) atomicCutOver() (err error) {
	atomic.StoreInt64(&mig.migrationContext.InCutOverCriticalSectionFlag, 1)
	defer atomic.StoreInt64(&mig.migrationContext.InCutOverCriticalSectionFlag, 0)

	okToUnlockTable := make(chan bool, 4)
	defer func() {
		okToUnlockTable <- true
	}()

	atomic.StoreInt64(&mig.migrationContext.AllEventsUpToLockProcessedInjectedFlag, 0)

	lockOriginalSessionIdChan := make(chan int64, 2)
	tableLocked := make(chan error, 2)
	tableUnlocked := make(chan error, 2)
	var renameLockSessionId int64
	go func() {
		if err := mig.applier.AtomicCutOverMagicLock(lockOriginalSessionIdChan, tableLocked, okToUnlockTable, tableUnlocked, &renameLockSessionId); err != nil {
			mig.migrationContext.Log.Errore(err)
		}
	}()
	if err := <-tableLocked; err != nil {
		return mig.migrationContext.Log.Errore(err)
	}
	lockOriginalSessionId := <-lockOriginalSessionIdChan
	mig.migrationContext.Log.Infof("Session locking original & magic tables is %+v", lockOriginalSessionId)
	// At this point we know the original table is locked.
	// We know any newly incoming DML on original table is blocked.
	if err := mig.waitForEventsUpToLock(); err != nil {
		return mig.migrationContext.Log.Errore(err)
	}

	// If we need to create triggers we need to do it here (only create part)
	if mig.migrationContext.IncludeTriggers && len(mig.migrationContext.Triggers) > 0 {
		if err := mig.applier.CreateTriggersOnGhost(); err != nil {
			return mig.migrationContext.Log.Errore(err)
		}
	}

	// Step 2
	// We now attempt an atomic RENAME on original & ghost tables, and expect it to block.
	mig.migrationContext.RenameTablesStartTime = time.Now()

	var tableRenameKnownToHaveFailed int64
	renameSessionIdChan := make(chan int64, 2)
	tablesRenamed := make(chan error, 2)
	go func() {
		if err := mig.applier.AtomicCutoverRename(renameSessionIdChan, tablesRenamed); err != nil {
			// Abort! Release the lock
			atomic.StoreInt64(&tableRenameKnownToHaveFailed, 1)
			okToUnlockTable <- true
		}
	}()
	renameSessionId := <-renameSessionIdChan
	mig.migrationContext.Log.Infof("Session renaming tables is %+v", renameSessionId)

	waitForRename := func() error {
		if atomic.LoadInt64(&tableRenameKnownToHaveFailed) == 1 {
			// We return `nil` here so as to avoid the `retry`. The RENAME has failed,
			// it won't show up in PROCESSLIST, no point in waiting
			return nil
		}
		return mig.applier.ExpectProcess(renameSessionId, "metadata lock", "rename")
	}
	// Wait for the RENAME to appear in PROCESSLIST
	if err := mig.retryOperation(waitForRename, true); err != nil {
		// Abort! Release the lock
		okToUnlockTable <- true
		return err
	}
	if atomic.LoadInt64(&tableRenameKnownToHaveFailed) == 0 {
		mig.migrationContext.Log.Infof("Found atomic RENAME to be blocking, as expected. Double checking the lock is still in place (though I don't strictly have to)")
	}
	if err := mig.applier.ExpectUsedLock(lockOriginalSessionId); err != nil {
		// Abort operation. Just make sure to drop the magic table.
		return mig.migrationContext.Log.Errore(err)
	}
	mig.migrationContext.Log.Infof("Connection holding lock on original table still exists")

	// Now that we've found the RENAME blocking, AND the locking connection still alive,
	// we know it is safe to proceed to release the lock

	renameLockSessionId = renameSessionId
	okToUnlockTable <- true
	// BAM! magic table dropped, original table lock is released
	// -> RENAME released -> queries on original are unblocked.
	if err := <-tableUnlocked; err != nil {
		return mig.migrationContext.Log.Errore(err)
	}
	if err := <-tablesRenamed; err != nil {
		return mig.migrationContext.Log.Errore(err)
	}
	mig.migrationContext.RenameTablesEndTime = time.Now()

	// ooh nice! We're actually truly and thankfully done
	lockAndRenameDuration := mig.migrationContext.RenameTablesEndTime.Sub(mig.migrationContext.LockTablesStartTime)
	mig.migrationContext.Log.Infof("Lock & rename duration: %s. During this time, queries on %s were blocked", lockAndRenameDuration, sql.EscapeName(mig.migrationContext.OriginalTableName))
	return nil
}

// initiateServer begins listening on unix socket/tcp for incoming interactive commands
func (mig *Migrator) initiateServer() (err error) {
	var printStatus printStatusFunc = func(rule PrintStatusRule, writer io.Writer) {
		mig.printStatus(rule, writer)
	}
	var printWorkers printWorkersFunc = func(writer io.Writer) {
		mig.printWorkerStats(writer)
	}
	mig.server = NewServer(mig.migrationContext, mig.hooksExecutor, printStatus, printWorkers)
	if err := mig.server.BindSocketFile(); err != nil {
		return err
	}
	if err := mig.server.BindTCPPort(); err != nil {
		return err
	}

	go mig.server.Serve()
	return nil
}

// initiateInspector connects, validates and inspects the "inspector" server.
// The "inspector" server is typically a replica; it is where we issue some
// queries such as:
// - table row count
// - schema validation
// - heartbeat
// When `--allow-on-master` is supplied, the inspector is actually the master.
func (mig *Migrator) initiateInspector() (err error) {
	mig.inspector = NewInspector(mig.migrationContext)
	if err := mig.inspector.InitDBConnections(); err != nil {
		return err
	}
	if err := mig.inspector.ValidateOriginalTable(); err != nil {
		return err
	}
	if err := mig.inspector.InspectOriginalTable(); err != nil {
		return err
	}
	// So far so good, table is accessible and valid.
	// Let's get master connection config
	if mig.migrationContext.AssumeMasterHostname == "" {
		// No forced master host; detect master
		if mig.migrationContext.ApplierConnectionConfig, err = mig.inspector.getMasterConnectionConfig(); err != nil {
			return err
		}
		mig.migrationContext.Log.Infof("Master found to be %+v", *mig.migrationContext.ApplierConnectionConfig.ImpliedKey)
	} else {
		// Forced master host.
		key, err := mysql.ParseInstanceKey(mig.migrationContext.AssumeMasterHostname)
		if err != nil {
			return err
		}
		mig.migrationContext.ApplierConnectionConfig = mig.migrationContext.InspectorConnectionConfig.DuplicateCredentials(*key)
		if mig.migrationContext.CliMasterUser != "" {
			mig.migrationContext.ApplierConnectionConfig.User = mig.migrationContext.CliMasterUser
		}
		if mig.migrationContext.CliMasterPassword != "" {
			mig.migrationContext.ApplierConnectionConfig.Password = mig.migrationContext.CliMasterPassword
		}
		if err := mig.migrationContext.ApplierConnectionConfig.RegisterTLSConfig(); err != nil {
			return err
		}
		mig.migrationContext.Log.Infof("Master forced to be %+v", *mig.migrationContext.ApplierConnectionConfig.ImpliedKey)
	}
	// validate configs
	if mig.migrationContext.TestOnReplica || mig.migrationContext.MigrateOnReplica {
		if mig.migrationContext.InspectorIsAlsoApplier() {
			return fmt.Errorf("instructed to --test-on-replica or --migrate-on-replica, but the server we connect to doesn't seem to be a replica")
		}
		mig.migrationContext.Log.Infof("--test-on-replica or --migrate-on-replica given. Will not execute on master %+v but rather on replica %+v itself",
			*mig.migrationContext.ApplierConnectionConfig.ImpliedKey, *mig.migrationContext.InspectorConnectionConfig.ImpliedKey,
		)
		mig.migrationContext.ApplierConnectionConfig = mig.migrationContext.InspectorConnectionConfig.Duplicate()
		if mig.migrationContext.GetThrottleControlReplicaKeys().Len() == 0 {
			mig.migrationContext.AddThrottleControlReplicaKey(mig.migrationContext.InspectorConnectionConfig.Key)
		}
	} else if mig.migrationContext.InspectorIsAlsoApplier() && !mig.migrationContext.AllowedRunningOnMaster {
		return ErrMigrationNotAllowedOnMaster
	}
	if err := mig.inspector.validateLogSlaveUpdates(); err != nil {
		return err
	}

	return nil
}

// initiateStatus sets and activates the printStatus() ticker
func (mig *Migrator) initiateStatus() {
	mig.printStatus(ForcePrintStatusAndHintRule)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var previousCount int64
	for range ticker.C {
		if atomic.LoadInt64(&mig.finishedMigrating) > 0 {
			return
		}
		go mig.printStatus(HeuristicPrintStatusRule)
		totalCopied := atomic.LoadInt64(&mig.migrationContext.TotalRowsCopied)
		if previousCount > 0 {
			copiedThisLoop := totalCopied - previousCount
			atomic.StoreInt64(&mig.migrationContext.EtaRowsPerSecond, copiedThisLoop)
		}
		previousCount = totalCopied
	}
}

// printMigrationStatusHint prints a detailed configuration dump, that is useful
// to keep in mind; such as the name of migrated table, throttle params etc.
// This gets printed at beginning and end of migration, every 10 minutes throughout
// migration, and as response to the "status" interactive command.
func (mig *Migrator) printMigrationStatusHint(writers ...io.Writer) {
	w := io.MultiWriter(writers...)
	fmt.Fprintf(w, "# Migrating %s.%s; Ghost table is %s.%s\n",
		sql.EscapeName(mig.migrationContext.DatabaseName),
		sql.EscapeName(mig.migrationContext.OriginalTableName),
		sql.EscapeName(mig.migrationContext.DatabaseName),
		sql.EscapeName(mig.migrationContext.GetGhostTableName()),
	)
	fmt.Fprintf(w, "# Migrating %+v; inspecting %+v; executing on %+v\n",
		*mig.applier.connectionConfig.ImpliedKey,
		*mig.inspector.connectionConfig.ImpliedKey,
		mig.migrationContext.Hostname,
	)
	fmt.Fprintf(w, "# Migration started at %+v\n",
		mig.migrationContext.StartTime.Format(time.RubyDate),
	)
	maxLoad := mig.migrationContext.GetMaxLoad()
	criticalLoad := mig.migrationContext.GetCriticalLoad()
	fmt.Fprintf(w, "# chunk-size: %+v; max-lag-millis: %+vms; dml-batch-size: %+v; max-load: %s; critical-load: %s; nice-ratio: %f\n",
		atomic.LoadInt64(&mig.migrationContext.ChunkSize),
		atomic.LoadInt64(&mig.migrationContext.MaxLagMillisecondsThrottleThreshold),
		atomic.LoadInt64(&mig.migrationContext.DMLBatchSize),
		maxLoad.String(),
		criticalLoad.String(),
		mig.migrationContext.GetNiceRatio(),
	)
	if mig.migrationContext.ThrottleFlagFile != "" {
		setIndicator := ""
		if base.FileExists(mig.migrationContext.ThrottleFlagFile) {
			setIndicator = "[set]"
		}
		fmt.Fprintf(w, "# throttle-flag-file: %+v %+v\n",
			mig.migrationContext.ThrottleFlagFile, setIndicator,
		)
	}
	if mig.migrationContext.ThrottleAdditionalFlagFile != "" {
		setIndicator := ""
		if base.FileExists(mig.migrationContext.ThrottleAdditionalFlagFile) {
			setIndicator = "[set]"
		}
		fmt.Fprintf(w, "# throttle-additional-flag-file: %+v %+v\n",
			mig.migrationContext.ThrottleAdditionalFlagFile, setIndicator,
		)
	}
	if throttleQuery := mig.migrationContext.GetThrottleQuery(); throttleQuery != "" {
		fmt.Fprintf(w, "# throttle-query: %+v\n",
			throttleQuery,
		)
	}
	if throttleControlReplicaKeys := mig.migrationContext.GetThrottleControlReplicaKeys(); throttleControlReplicaKeys.Len() > 0 {
		fmt.Fprintf(w, "# throttle-control-replicas count: %+v\n",
			throttleControlReplicaKeys.Len(),
		)
	}

	if mig.migrationContext.PostponeCutOverFlagFile != "" {
		setIndicator := ""
		if base.FileExists(mig.migrationContext.PostponeCutOverFlagFile) {
			setIndicator = "[set]"
		}
		fmt.Fprintf(w, "# postpone-cut-over-flag-file: %+v %+v\n",
			mig.migrationContext.PostponeCutOverFlagFile, setIndicator,
		)
	}
	if mig.migrationContext.PanicFlagFile != "" {
		fmt.Fprintf(w, "# panic-flag-file: %+v\n",
			mig.migrationContext.PanicFlagFile,
		)
	}
	fmt.Fprintf(w, "# Serving on unix socket: %+v\n",
		mig.migrationContext.ServeSocketFile,
	)
	if mig.migrationContext.ServeTCPPort != 0 {
		fmt.Fprintf(w, "# Serving on TCP port: %+v\n", mig.migrationContext.ServeTCPPort)
	}
}

// getProgressPercent returns an estimate of migration progess as a percent.
func (mig *Migrator) getProgressPercent(rowsEstimate int64) (progressPct float64) {
	progressPct = 100.0
	if rowsEstimate > 0 {
		progressPct *= float64(mig.migrationContext.GetTotalRowsCopied()) / float64(rowsEstimate)
	}
	return progressPct
}

// getMigrationETA returns the estimated duration of the migration
func (mig *Migrator) getMigrationETA(rowsEstimate int64) (eta string, duration time.Duration) {
	duration = time.Duration(base.ETAUnknown)
	progressPct := mig.getProgressPercent(rowsEstimate)
	if progressPct >= 100.0 {
		duration = 0
	} else if progressPct >= 0.1 {
		totalRowsCopied := mig.migrationContext.GetTotalRowsCopied()
		etaRowsPerSecond := atomic.LoadInt64(&mig.migrationContext.EtaRowsPerSecond)
		var etaSeconds float64
		// If there is data available on our current row-copies-per-second rate, use it.
		// Otherwise we can fallback to the total elapsed time and extrapolate.
		// This is going to be less accurate on a longer copy as the insert rate
		// will tend to slow down.
		if etaRowsPerSecond > 0 {
			remainingRows := float64(rowsEstimate) - float64(totalRowsCopied)
			etaSeconds = remainingRows / float64(etaRowsPerSecond)
		} else {
			elapsedRowCopySeconds := mig.migrationContext.ElapsedRowCopyTime().Seconds()
			totalExpectedSeconds := elapsedRowCopySeconds * float64(rowsEstimate) / float64(totalRowsCopied)
			etaSeconds = totalExpectedSeconds - elapsedRowCopySeconds
		}
		if etaSeconds >= 0 {
			duration = time.Duration(etaSeconds) * time.Second
		} else {
			duration = 0
		}
	}

	switch duration {
	case 0:
		eta = "due"
	case time.Duration(base.ETAUnknown):
		eta = "N/A"
	default:
		eta = base.PrettifyDurationOutput(duration)
	}

	return eta, duration
}

// getMigrationStateAndETA returns the state and eta of the migration.
func (mig *Migrator) getMigrationStateAndETA(rowsEstimate int64) (state, eta string, etaDuration time.Duration) {
	eta, etaDuration = mig.getMigrationETA(rowsEstimate)
	state = "migrating"
	if atomic.LoadInt64(&mig.migrationContext.CountingRowsFlag) > 0 && !mig.migrationContext.ConcurrentCountTableRows {
		state = "counting rows"
	} else if atomic.LoadInt64(&mig.migrationContext.IsPostponingCutOver) > 0 {
		eta = "due"
		state = "postponing cut-over"
	} else if isThrottled, throttleReason, _ := mig.migrationContext.IsThrottled(); isThrottled {
		state = fmt.Sprintf("throttled, %s", throttleReason)
	}
	return state, eta, etaDuration
}

// shouldPrintStatus returns true when the migrator is due to print status info.
func (mig *Migrator) shouldPrintStatus(rule PrintStatusRule, elapsedSeconds int64, etaDuration time.Duration) (shouldPrint bool) {
	if rule != HeuristicPrintStatusRule {
		return true
	}

	etaSeconds := etaDuration.Seconds()
	if elapsedSeconds <= 60 {
		shouldPrint = true
	} else if etaSeconds <= 60 {
		shouldPrint = true
	} else if etaSeconds <= 180 {
		shouldPrint = (elapsedSeconds%5 == 0)
	} else if elapsedSeconds <= 180 {
		shouldPrint = (elapsedSeconds%5 == 0)
	} else if mig.migrationContext.TimeSincePointOfInterest().Seconds() <= 60 {
		shouldPrint = (elapsedSeconds%5 == 0)
	} else {
		shouldPrint = (elapsedSeconds%30 == 0)
	}

	return shouldPrint
}

// shouldPrintMigrationStatus returns true when the migrator is due to print the migration status hint
func (mig *Migrator) shouldPrintMigrationStatusHint(rule PrintStatusRule, elapsedSeconds int64) (shouldPrint bool) {
	if elapsedSeconds%600 == 0 {
		shouldPrint = true
	} else if rule == ForcePrintStatusAndHintRule {
		shouldPrint = true
	}
	return shouldPrint
}

// printWorkerStats prints cumulative stats from the trxCoordinator workers.
func (mig *Migrator) printWorkerStats(writers ...io.Writer) {
	writers = append(writers, os.Stdout)
	mw := io.MultiWriter(writers...)

	busyWorkers := mig.trxCoordinator.busyWorkers.Load()
	totalWorkers := cap(mig.trxCoordinator.workerQueue)
	fmt.Fprintf(mw, "# %d/%d workers are busy\n", busyWorkers, totalWorkers)

	stats := mig.trxCoordinator.GetWorkerStats()
	for id, stat := range stats {
		fmt.Fprintf(mw,
			"Worker %d; Waited: %s; Busy: %s; DML Applied: %d (%.2f/s), Trx Applied: %d (%.2f/s)\n",
			id,
			base.PrettifyDurationOutput(stat.waitTime),
			base.PrettifyDurationOutput(stat.busyTime),
			stat.dmlEventsApplied,
			stat.dmlRate,
			stat.executedJobs,
			stat.trxRate)
	}
}

// printStatus prints the progress status, and optionally additionally detailed
// dump of configuration.
// `rule` indicates the type of output expected.
// By default the status is written to standard output, but other writers can
// be used as well.
func (mig *Migrator) printStatus(rule PrintStatusRule, writers ...io.Writer) {
	if rule == NoPrintStatusRule {
		return
	}
	writers = append(writers, os.Stdout)

	elapsedTime := mig.migrationContext.ElapsedTime()
	elapsedSeconds := int64(elapsedTime.Seconds())
	totalRowsCopied := mig.migrationContext.GetTotalRowsCopied()
	rowsEstimate := atomic.LoadInt64(&mig.migrationContext.RowsEstimate) + atomic.LoadInt64(&mig.migrationContext.RowsDeltaEstimate)
	if atomic.LoadInt64(&mig.rowCopyCompleteFlag) == 1 {
		// Done copying rows. The totalRowsCopied value is the de-facto number of rows,
		// and there is no further need to keep updating the value.
		rowsEstimate = totalRowsCopied
	}

	// we take the opportunity to update migration context with progressPct
	progressPct := mig.getProgressPercent(rowsEstimate)
	mig.migrationContext.SetProgressPct(progressPct)

	// Before status, let's see if we should print a nice reminder for what exactly we're doing here.
	if mig.shouldPrintMigrationStatusHint(rule, elapsedSeconds) {
		mig.printMigrationStatusHint(writers...)
	}

	// Get state + ETA
	state, eta, etaDuration := mig.getMigrationStateAndETA(rowsEstimate)
	mig.migrationContext.SetETADuration(etaDuration)

	if !mig.shouldPrintStatus(rule, elapsedSeconds, etaDuration) {
		return
	}

	currentBinlogCoordinates := mig.trxCoordinator.binlogReader.GetCurrentBinlogCoordinates()

	status := fmt.Sprintf("Copy: %d/%d %.1f%%; Applied: %d; Backlog: %d/%d; Time: %+v(total), %+v(copy); streamer: %+v; Lag: %.2fs, HeartbeatLag: %.2fs, State: %s; ETA: %s",
		totalRowsCopied, rowsEstimate, progressPct,
		atomic.LoadInt64(&mig.migrationContext.TotalDMLEventsApplied),
		len(mig.trxCoordinator.events), cap(mig.trxCoordinator.events),
		base.PrettifyDurationOutput(elapsedTime), base.PrettifyDurationOutput(mig.migrationContext.ElapsedRowCopyTime()),
		currentBinlogCoordinates.DisplayString(),
		mig.migrationContext.GetCurrentLagDuration().Seconds(),
		mig.migrationContext.TimeSinceLastHeartbeatOnChangelog().Seconds(),
		state,
		eta,
	)
	mig.applier.WriteChangelog(
		fmt.Sprintf("copy iteration %d at %d", mig.migrationContext.GetIteration(), time.Now().Unix()),
		state,
	)
	w := io.MultiWriter(writers...)
	fmt.Fprintln(w, status)

	// This "hack" is required here because the underlying logging library
	// github.com/outbrain/golib/log provides two functions Info and Infof; but the arguments of
	// both these functions are eventually redirected to the same function, which internally calls
	// fmt.Sprintf. So, the argument of every function called on the DefaultLogger object
	// migrationContext.Log will eventually pass through fmt.Sprintf, and thus the '%' character
	// needs to be escaped.
	mig.migrationContext.Log.Info(strings.Replace(status, "%", "%%", 1))

	hooksStatusIntervalSec := mig.migrationContext.HooksStatusIntervalSec
	if hooksStatusIntervalSec > 0 && elapsedSeconds%hooksStatusIntervalSec == 0 {
		mig.hooksExecutor.OnStatus(status)
	}
}

// initiateStreaming begins streaming of binary log events and registers listeners for such events
func (mig *Migrator) initiateStreaming() error {
	initialCoords, err := mig.inspector.readCurrentBinlogCoordinates()
	if err != nil {
		return err
	}

	go func() {
		mig.migrationContext.Log.Debugf("Beginning streaming at coordinates: %+v", initialCoords)
		ctx := context.TODO()
		err := mig.trxCoordinator.StartStreaming(ctx, initialCoords, mig.canStopStreaming)
		if err != nil {
			// Use helper to prevent deadlock if listenOnPanicAbort already exited
			_ = base.SendWithContext(mig.migrationContext.GetContext(), mig.migrationContext.PanicAbort, err)
		}
		mig.migrationContext.Log.Debugf("Done streaming")
	}()

	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if atomic.LoadInt64(&mig.finishedMigrating) > 0 {
				return
			}
			mig.migrationContext.SetRecentBinlogCoordinates(mig.trxCoordinator.binlogReader.GetCurrentBinlogCoordinates())
		}
	}()
	return nil
}

// initiateThrottler kicks in the throttling collection and the throttling checks.
func (mig *Migrator) initiateThrottler() {
	mig.throttler = NewThrottler(mig.migrationContext, mig.applier, mig.inspector, mig.appVersion)

	go mig.throttler.initiateThrottlerCollection(mig.firstThrottlingCollected)
	mig.migrationContext.Log.Infof("Waiting for first throttle metrics to be collected")
	<-mig.firstThrottlingCollected // replication lag
	<-mig.firstThrottlingCollected // HTTP status
	<-mig.firstThrottlingCollected // other, general metrics
	mig.migrationContext.Log.Infof("First throttle metrics collected")
	go mig.throttler.initiateThrottlerChecks()
}

func (mig *Migrator) initiateApplier() error {
	mig.applier = NewApplier(mig.migrationContext)
	if err := mig.applier.InitDBConnections(mig.migrationContext.NumWorkers); err != nil {
		return err
	}
	if mig.migrationContext.Revert {
		if err := mig.applier.CreateChangelogTable(); err != nil {
			mig.migrationContext.Log.Errorf("Unable to create changelog table, see further error details. Perhaps a previous migration failed without dropping the table? OR is there a running migration? Bailing out")
			return err
		}
	} else if !mig.migrationContext.Resume {
		if err := mig.applier.ValidateOrDropExistingTables(); err != nil {
			return err
		}
		if err := mig.applier.CreateChangelogTable(); err != nil {
			mig.migrationContext.Log.Errorf("Unable to create changelog table, see further error details. Perhaps a previous migration failed without dropping the table? OR is there a running migration? Bailing out")
			return err
		}
		if err := mig.applier.CreateGhostTable(); err != nil {
			mig.migrationContext.Log.Errorf("Unable to create ghost table, see further error details. Perhaps a previous migration failed without dropping the table? Bailing out")
			return err
		}
		if err := mig.applier.AlterGhost(); err != nil {
			mig.migrationContext.Log.Errorf("Unable to ALTER ghost table, see further error details. Bailing out")
			return err
		}

		if mig.migrationContext.OriginalTableAutoIncrement > 0 && !mig.parser.IsAutoIncrementDefined() {
			// Original table has AUTO_INCREMENT value and the -alter statement does not indicate any override,
			// so we should copy AUTO_INCREMENT value onto our ghost table.
			if err := mig.applier.AlterGhostAutoIncrement(); err != nil {
				mig.migrationContext.Log.Errorf("Unable to ALTER ghost table AUTO_INCREMENT value, see further error details. Bailing out")
				return err
			}
		}
		if _, err := mig.applier.WriteChangelogState(string(GhostTableMigrated)); err != nil {
			return err
		}
	}

	// ensure performance_schema.metadata_locks is available.
	if err := mig.applier.StateMetadataLockInstrument(); err != nil {
		mig.migrationContext.Log.Warning("Unable to enable metadata lock instrument, see further error details.")
	}
	if !mig.migrationContext.IsOpenMetadataLockInstruments {
		if !mig.migrationContext.SkipMetadataLockCheck {
			return mig.migrationContext.Log.Errorf("Bailing out because metadata lock instrument not enabled. Use --skip-metadata-lock-check if you wish to proceed without. See https://github.com/github/gh-ost/pull/1536 for details.")
		}
		mig.migrationContext.Log.Warning("Proceeding without metadata lock check. There is a small chance of data loss if another session accesses the ghost table during cut-over. See https://github.com/github/gh-ost/pull/1536 for details.")
	}

	go mig.applier.InitiateHeartbeat()
	return nil
}

// iterateChunks iterates the existing table rows, and generates a copy task of
// a chunk of rows onto the ghost table.
func (mig *Migrator) iterateChunks() error {
	terminateRowIteration := func(err error) error {
		_ = base.SendWithContext(mig.migrationContext.GetContext(), mig.rowCopyComplete, err)
		return mig.migrationContext.Log.Errore(err)
	}
	if mig.migrationContext.Noop {
		mig.migrationContext.Log.Debugf("Noop operation; not really copying data")
		return terminateRowIteration(nil)
	}
	if mig.migrationContext.MigrationRangeMinValues == nil {
		mig.migrationContext.Log.Debugf("No rows found in table. Rowcopy will be implicitly empty")
		return terminateRowIteration(nil)
	}

	var hasNoFurtherRangeFlag int64
	// Iterate per chunk:
	for {
		if err := mig.checkAbort(); err != nil {
			return terminateRowIteration(err)
		}
		if atomic.LoadInt64(&mig.rowCopyCompleteFlag) == 1 || atomic.LoadInt64(&hasNoFurtherRangeFlag) == 1 {
			// Done
			// There's another such check down the line
			return nil
		}
		copyRowsFunc := func() error {
			mig.migrationContext.SetNextIterationRangeMinValues()
			// Copy task:
			applyCopyRowsFunc := func() error {
				if atomic.LoadInt64(&mig.rowCopyCompleteFlag) == 1 || atomic.LoadInt64(&hasNoFurtherRangeFlag) == 1 {
					// Done.
					// There's another such check down the line
					return nil
				}

				// When hasFurtherRange is false, original table might be write locked and CalculateNextIterationRangeEndValues would hangs forever
				hasFurtherRange, err := mig.applier.CalculateNextIterationRangeEndValues()
				if err != nil {
					return err // wrapping call will retry
				}
				if !hasFurtherRange {
					atomic.StoreInt64(&hasNoFurtherRangeFlag, 1)
					return terminateRowIteration(nil)
				}
				if atomic.LoadInt64(&mig.rowCopyCompleteFlag) == 1 {
					// No need for more writes.
					// This is the de-facto place where we avoid writing in the event of completed cut-over.
					// There could _still_ be a race condition, but that's as close as we can get.
					// What about the race condition? Well, there's actually no data integrity issue.
					// when rowCopyCompleteFlag==1 that means **guaranteed** all necessary rows have been copied.
					// But some are still then collected at the binary log, and these are the ones we're trying to
					// not apply here. If the race condition wins over us, then we just attempt to apply onto the
					// _ghost_ table, which no longer exists. So, bothering error messages and all, but no damage.
					return nil
				}
				_, rowsAffected, _, err := mig.applier.ApplyIterationInsertQuery()
				if err != nil {
					return err // wrapping call will retry
				}

				if mig.migrationContext.PanicOnWarnings {
					if len(mig.migrationContext.MigrationLastInsertSQLWarnings) > 0 {
						for _, warning := range mig.migrationContext.MigrationLastInsertSQLWarnings {
							mig.migrationContext.Log.Infof("ApplyIterationInsertQuery has SQL warnings! %s", warning)
						}
						joinedWarnings := strings.Join(mig.migrationContext.MigrationLastInsertSQLWarnings, "; ")
						return terminateRowIteration(fmt.Errorf("ApplyIterationInsertQuery failed because of SQL warnings: [%s]", joinedWarnings))
					}
				}

				atomic.AddInt64(&mig.migrationContext.TotalRowsCopied, rowsAffected)
				atomic.AddInt64(&mig.migrationContext.Iteration, 1)
				return nil
			}
			if err := mig.retryBatchCopyWithHooks(applyCopyRowsFunc); err != nil {
				return terminateRowIteration(err)
			}

			// record last successfully copied range
			mig.applier.LastIterationRangeMutex.Lock()
			if mig.migrationContext.MigrationIterationRangeMinValues != nil && mig.migrationContext.MigrationIterationRangeMaxValues != nil {
				mig.applier.LastIterationRangeMinValues = mig.migrationContext.MigrationIterationRangeMinValues.Clone()
				mig.applier.LastIterationRangeMaxValues = mig.migrationContext.MigrationIterationRangeMaxValues.Clone()
			}
			mig.applier.LastIterationRangeMutex.Unlock()

			return nil
		}
		// Enqueue copy operation; to be executed by executeWriteFuncs()
		// Use helper to prevent deadlock if executeWriteFuncs exits
		if err := base.SendWithContext(mig.migrationContext.GetContext(), mig.copyRowsQueue, copyRowsFunc); err != nil {
			// Context cancelled, check for abort and exit
			if abortErr := mig.checkAbort(); abortErr != nil {
				return terminateRowIteration(abortErr)
			}
			return terminateRowIteration(err)
		}
	}
}

// Checkpoint attempts to write a checkpoint of the Migrator's current state.
// It gets the binlog coordinates of the last received trx and waits until the
// applier reaches that trx. At that point it's safe to resume from these coordinates.
func (mig *Migrator) Checkpoint(ctx context.Context) (*Checkpoint, error) {
	coords := mig.trxCoordinator.binlogReader.GetCurrentBinlogCoordinates()
	mig.applier.LastIterationRangeMutex.Lock()
	if mig.applier.LastIterationRangeMaxValues == nil || mig.applier.LastIterationRangeMinValues == nil {
		mig.applier.LastIterationRangeMutex.Unlock()
		return nil, errors.New("iteration range is empty, not checkpointing")
	}
	chk := &Checkpoint{
		Iteration:         mig.migrationContext.GetIteration(),
		IterationRangeMin: mig.applier.LastIterationRangeMinValues.Clone(),
		IterationRangeMax: mig.applier.LastIterationRangeMaxValues.Clone(),
		LastTrxCoords:     coords,
		RowsCopied:        atomic.LoadInt64(&mig.migrationContext.TotalRowsCopied),
		DMLApplied:        atomic.LoadInt64(&mig.migrationContext.TotalDMLEventsApplied),
	}
	mig.applier.LastIterationRangeMutex.Unlock()

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		mig.applier.CurrentCoordinatesMutex.Lock()
		if coords.SmallerThanOrEquals(mig.applier.CurrentCoordinates) {
			id, err := mig.applier.WriteCheckpoint(chk)
			chk.Id = id
			mig.applier.CurrentCoordinatesMutex.Unlock()
			return chk, err
		}
		mig.applier.CurrentCoordinatesMutex.Unlock()
		time.Sleep(500 * time.Millisecond)
	}
}

// CheckpointAfterCutOver writes a final checkpoint after the cutover completes successfully.
func (mig *Migrator) CheckpointAfterCutOver() (*Checkpoint, error) {
	if mig.lastLockProcessed == nil || mig.lastLockProcessed.coords.IsEmpty() {
		return nil, mig.migrationContext.Log.Errorf("lastLockProcessed coords are empty")
	}

	chk := &Checkpoint{
		IsCutover:         true,
		LastTrxCoords:     mig.lastLockProcessed.coords,
		IterationRangeMin: sql.NewColumnValues(mig.migrationContext.UniqueKey.Len()),
		IterationRangeMax: sql.NewColumnValues(mig.migrationContext.UniqueKey.Len()),
		Iteration:         mig.migrationContext.GetIteration(),
		RowsCopied:        atomic.LoadInt64(&mig.migrationContext.TotalRowsCopied),
		DMLApplied:        atomic.LoadInt64(&mig.migrationContext.TotalDMLEventsApplied),
	}
	mig.applier.LastIterationRangeMutex.Lock()
	if mig.applier.LastIterationRangeMinValues != nil {
		chk.IterationRangeMin = mig.applier.LastIterationRangeMinValues.Clone()
	}
	if mig.applier.LastIterationRangeMaxValues != nil {
		chk.IterationRangeMax = mig.applier.LastIterationRangeMaxValues.Clone()
	}
	mig.applier.LastIterationRangeMutex.Unlock()

	id, err := mig.applier.WriteCheckpoint(chk)
	chk.Id = id
	return chk, err
}

func (mig *Migrator) checkpointLoop() {
	if mig.migrationContext.Noop {
		mig.migrationContext.Log.Debugf("Noop operation; not really checkpointing")
		return
	}
	checkpointInterval := time.Duration(mig.migrationContext.CheckpointIntervalSeconds) * time.Second
	ticker := time.NewTicker(checkpointInterval)
	for t := range ticker.C {
		if atomic.LoadInt64(&mig.finishedMigrating) > 0 || atomic.LoadInt64(&mig.migrationContext.CutOverCompleteFlag) > 0 {
			return
		}
		if atomic.LoadInt64(&mig.migrationContext.InCutOverCriticalSectionFlag) > 0 {
			continue
		}
		mig.migrationContext.Log.Infof("starting checkpoint at %+v", t)
		ctx, cancel := context.WithTimeout(context.Background(), checkpointTimeout)
		chk, err := mig.Checkpoint(ctx)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				mig.migrationContext.Log.Errorf("checkpoint attempt timed out after %+v", checkpointTimeout)
			} else {
				mig.migrationContext.Log.Errorf("error attempting checkpoint: %+v", err)
			}
		} else {
			mig.migrationContext.Log.Infof("checkpoint success at coords=%+v range_min=%+v range_max=%+v iteration=%d",
				chk.LastTrxCoords.DisplayString(), chk.IterationRangeMin.String(), chk.IterationRangeMax.String(), chk.Iteration)
		}
		cancel()
	}
}

// executeWriteFuncs writes data via applier: both the rowcopy and the events backlog.
// This is where the ghost table gets the data. The function fills the data single-threaded.
// Both event backlog and rowcopy events are polled; the backlog events have precedence.
func (mig *Migrator) executeWriteFuncs() error {
	if mig.migrationContext.Noop {
		mig.migrationContext.Log.Debugf("Noop operation; not really executing write funcs")
		return nil
	}

	for {
		if err := mig.checkAbort(); err != nil {
			return err
		}
		if atomic.LoadInt64(&mig.finishedMigrating) > 0 {
			return nil
		}

		mig.throttler.throttle(nil)

		// We give higher priority to event processing.
		// ProcessEventsUntilDrained will process all events in the queue, and then return once no more events are available.
		if err := mig.trxCoordinator.ProcessEventsUntilDrained(); err != nil {
			return mig.migrationContext.Log.Errore(err)
		}

		mig.throttler.throttle(nil)

		// And secondary priority to rowcopy
		select {
		case copyRowsFunc := <-mig.copyRowsQueue:
			{
				copyRowsStartTime := time.Now()
				// Retries are handled within the copyRowsFunc
				if err := copyRowsFunc(); err != nil {
					return mig.migrationContext.Log.Errore(err)
				}
				if niceRatio := mig.migrationContext.GetNiceRatio(); niceRatio > 0 {
					copyRowsDuration := time.Since(copyRowsStartTime)
					sleepTimeNanosecondFloat64 := niceRatio * float64(copyRowsDuration.Nanoseconds())
					sleepTime := time.Duration(int64(sleepTimeNanosecondFloat64)) * time.Nanosecond
					time.Sleep(sleepTime)
				}
			}
		default:
			{
				// Hmmmmm... nothing in the queue; no events, but also no row copy.
				// This is possible upon load. Let's just sleep it over.
				mig.migrationContext.Log.Debugf("Getting nothing in the write queue. Sleeping...")
				time.Sleep(time.Second)
			}
		}
	}
}

// finalCleanup takes actions at very end of migration, dropping tables etc.
func (mig *Migrator) finalCleanup() error {
	atomic.StoreInt64(&mig.migrationContext.CleanupImminentFlag, 1)

	mig.migrationContext.Log.Infof("Writing changelog state: %+v", Migrated)
	if _, err := mig.applier.WriteChangelogState(string(Migrated)); err != nil {
		return err
	}

	if mig.migrationContext.Noop {
		if createTableStatement, err := mig.inspector.showCreateTable(mig.migrationContext.GetGhostTableName()); err == nil {
			mig.migrationContext.Log.Infof("New table structure follows")
			fmt.Println(createTableStatement)
		} else {
			mig.migrationContext.Log.Errore(err)
		}
	}
	if err := mig.retryOperation(mig.applier.DropChangelogTable); err != nil {
		return err
	}
	if mig.migrationContext.OkToDropTable && !mig.migrationContext.TestOnReplica {
		if err := mig.retryOperation(mig.applier.DropOldTable); err != nil {
			return err
		}
		if err := mig.retryOperation(mig.applier.DropCheckpointTable); err != nil {
			return err
		}
	} else if !mig.migrationContext.Noop {
		mig.migrationContext.Log.Infof("Am not dropping old table because I want this operation to be as live as possible. If you insist I should do it, please add `--ok-to-drop-table` next time. But I prefer you do not. To drop the old table, issue:")
		mig.migrationContext.Log.Infof("-- drop table %s.%s", sql.EscapeName(mig.migrationContext.DatabaseName), sql.EscapeName(mig.migrationContext.GetOldTableName()))
		if mig.migrationContext.Checkpoint {
			mig.migrationContext.Log.Infof("Am not dropping checkpoint table without `--ok-to-drop-table`. To drop the checkpoint table, issue:")
			mig.migrationContext.Log.Infof("-- drop table %s.%s", sql.EscapeName(mig.migrationContext.DatabaseName), sql.EscapeName(mig.migrationContext.GetCheckpointTableName()))
		}
	}
	if mig.migrationContext.Noop {
		if err := mig.retryOperation(mig.applier.DropGhostTable); err != nil {
			return err
		}
	}

	return nil
}

func (mig *Migrator) teardown() {
	atomic.StoreInt64(&mig.finishedMigrating, 1)

	if mig.trxCoordinator != nil {
		mig.migrationContext.Log.Infof("Tearing down coordinator")
		mig.trxCoordinator.Teardown()
	}

	if mig.throttler != nil {
		mig.migrationContext.Log.Infof("Tearing down throttler")
		mig.throttler.Teardown()
	}

	if mig.inspector != nil {
		mig.migrationContext.Log.Infof("Tearing down inspector")
		mig.inspector.Teardown()
	}

	if mig.applier != nil {
		mig.migrationContext.Log.Infof("Tearing down applier")
		mig.applier.Teardown()
	}
}
