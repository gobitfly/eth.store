GITURL="github.com/gobitfly/eth.store"
GITCOMMIT=`git describe --always`
GITDATE=$$(TZ=UTC git show -s --date=iso-strict-local --format=%cd HEAD | sed 's/[-T:]//g' | sed 's/\(+.*\)$$//g')
BUILDDATE=`date -u +"%Y%m%d%H%M%S"`
LDFLAGS="-X ${GITURL}/version.GitCommit=${GITCOMMIT} -X ${GITURL}/version.GitDate=${GITDATE} -X ${GITURL}/version.Version=${GITDATE}-${GITCOMMIT}"
BINARY=bin/eth.store
all: test build
test:
	go test -v ./...
clean:
	rm -rf bin
build:
	go build --ldflags=${LDFLAGS} -o ${BINARY} ./cmd
	${BINARY} -version
build-linux-amd64:
	env GOOS=linux GOARCH=amd64 go build --ldflags=${LDFLAGS} -o ${BINARY}-linux-amd64 ./cmd
build-linux-arm64:
	env GOOS=linux GOARCH=arm64 go build --ldflags=${LDFLAGS} -o ${BINARY}-linux-arm64 ./cmd
