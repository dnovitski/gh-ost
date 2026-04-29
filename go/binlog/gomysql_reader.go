/*
   Copyright 2022 GitHub Inc.
 See https://github.com/github/gh-ost/blob/master/LICENSE
*/

package binlog

import (
	"fmt"
	"sync"

	"github.com/github/gh-ost/go/base"
	"github.com/github/gh-ost/go/mysql"

	"time"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
	uuid "github.com/google/uuid"
	"golang.org/x/net/context"
)

type GoMySQLReader struct {
	migrationContext        *base.MigrationContext
	connectionConfig        *mysql.ConnectionConfig
	binlogSyncer            *replication.BinlogSyncer
	binlogStreamer          *replication.BinlogStreamer
	currentCoordinates      mysql.BinlogCoordinates
	currentCoordinatesMutex *sync.Mutex
	// LastTrxCoords are the coordinates of the last transaction completely read.
	// If using the file coordinates it is binlog position of the transaction's XID event.
	LastTrxCoords mysql.BinlogCoordinates
}

func NewGoMySQLReader(migrationContext *base.MigrationContext) *GoMySQLReader {
	connectionConfig := migrationContext.InspectorConnectionConfig
	return &GoMySQLReader{
		migrationContext:        migrationContext,
		connectionConfig:        connectionConfig,
		currentCoordinatesMutex: &sync.Mutex{},
		binlogSyncer: replication.NewBinlogSyncer(replication.BinlogSyncerConfig{
			ServerID:                uint32(migrationContext.ReplicaServerId),
			Flavor:                  gomysql.MySQLFlavor,
			Host:                    connectionConfig.Key.Hostname,
			Port:                    uint16(connectionConfig.Key.Port),
			User:                    connectionConfig.User,
			Password:                connectionConfig.Password,
			TLSConfig:               connectionConfig.TLSConfig(),
			UseDecimal:              true,
			TimestampStringLocation: time.UTC,
			MaxReconnectAttempts:    migrationContext.BinlogSyncerMaxReconnectAttempts,
		}),
	}
}

// ConnectBinlogStreamer
func (this *GoMySQLReader) ConnectBinlogStreamer(coordinates mysql.BinlogCoordinates) (err error) {
	if coordinates.IsEmpty() {
		return this.migrationContext.Log.Errorf("Empty coordinates at ConnectBinlogStreamer()")
	}

	this.currentCoordinatesMutex.Lock()
	defer this.currentCoordinatesMutex.Unlock()
	this.currentCoordinates = coordinates
	this.migrationContext.Log.Infof("Connecting binlog streamer at %+v", coordinates)

	// Start sync with specified GTID set or binlog file and position
	if this.migrationContext.UseGTIDs {
		coords := coordinates.(*mysql.GTIDBinlogCoordinates)
		this.binlogStreamer, err = this.binlogSyncer.StartSyncGTID(coords.GTIDSet)
	} else {
		coords := this.currentCoordinates.(*mysql.FileBinlogCoordinates)
		this.binlogStreamer, err = this.binlogSyncer.StartSync(gomysql.Position{
			Name: coords.LogFile,
			Pos:  uint32(coords.LogPos)},
		)
	}
	return err
}

func (this *GoMySQLReader) GetCurrentBinlogCoordinates() mysql.BinlogCoordinates {
	this.currentCoordinatesMutex.Lock()
	defer this.currentCoordinatesMutex.Unlock()
	return this.currentCoordinates.Clone()
}

// StreamEvents reads binlog events and sends them to the given channel.
// It is blocking and should be executed in a goroutine.
func (this *GoMySQLReader) StreamEvents(ctx context.Context, canStopStreaming func() bool, eventChannel chan<- *replication.BinlogEvent) error {
	for {
		if canStopStreaming() {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		ev, err := this.binlogStreamer.GetEvent(ctx)
		if err != nil {
			return err
		}

		// Update binlog coords if using file-based coords.
		// GTID coordinates are updated on receiving GTID events.
		if !this.migrationContext.UseGTIDs {
			this.currentCoordinatesMutex.Lock()
			coords := this.currentCoordinates.(*mysql.FileBinlogCoordinates)
			prevCoords := coords.Clone().(*mysql.FileBinlogCoordinates)
			coords.LogPos = int64(ev.Header.LogPos)
			coords.EventSize = int64(ev.Header.EventSize)
			if coords.IsLogPosOverflowBeyond4Bytes(prevCoords) {
				this.currentCoordinatesMutex.Unlock()
				return fmt.Errorf("Unexpected rows event at %+v, the binlog end_log_pos is overflow 4 bytes", coords)
			}
			this.currentCoordinatesMutex.Unlock()
		}

		switch event := ev.Event.(type) {
		case *replication.GTIDEvent:
			if this.migrationContext.UseGTIDs {
				sid, err := uuid.FromBytes(event.SID)
				if err != nil {
					return err
				}
				this.currentCoordinatesMutex.Lock()
				if this.LastTrxCoords != nil {
					this.currentCoordinates = this.LastTrxCoords.Clone()
				}
				coords := this.currentCoordinates.(*mysql.GTIDBinlogCoordinates)
				trxGset := gomysql.NewUUIDSet(sid, gomysql.Interval{Start: event.GNO, Stop: event.GNO + 1})
				coords.GTIDSet.AddSet(trxGset)
				this.currentCoordinatesMutex.Unlock()
			}
		case *replication.RotateEvent:
			if !this.migrationContext.UseGTIDs {
				this.currentCoordinatesMutex.Lock()
				coords := this.currentCoordinates.(*mysql.FileBinlogCoordinates)
				coords.LogFile = string(event.NextLogName)
				this.migrationContext.Log.Infof("rotate to next log from %s:%d to %s", coords.LogFile, int64(ev.Header.LogPos), event.NextLogName)
				this.currentCoordinatesMutex.Unlock()
			}
		case *replication.XIDEvent:
			if this.migrationContext.UseGTIDs {
				this.LastTrxCoords = &mysql.GTIDBinlogCoordinates{GTIDSet: event.GSet.(*gomysql.MysqlGTIDSet)}
			} else {
				this.LastTrxCoords = this.currentCoordinates.Clone()
			}
		}

		eventChannel <- ev
	}
}

func (this *GoMySQLReader) Close() error {
	this.binlogSyncer.Close()
	return nil
}
