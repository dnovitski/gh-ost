docker run -ti --rm --name gh-ost-packager -v `pwd`:/tmp/go/src/github.com/github/gh-ost -v `pwd`/release:/tmp/gh-ost-release docker.io/library/gh-ost-packager:latest sh -c "./build.sh"
