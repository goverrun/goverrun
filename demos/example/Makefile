.PHONY: help local upgrade test install cross-compile-linux cross-compile-mac cross-compile-windows clean


help: Makefile # parse the double-# tagged lines into help
	@echo "Choose a command:"
	@sed -n 's/^##//p' $< | column -t -s ':' |  sed -e 's/^/ /'


## local: Shortcut for upgrade, test, local build, and install
local: upgrade test install


## upgrade: upgrade dependencies
upgrade:
	go mod edit -go=1.17 # set the minimum go version to use for the module
	go mod tidy # add missing and remove unused modules
	go get -u ./... # update all dependencies (also in sub-dirs) -- but only minor and patch version updates (manually edit go.mod for major version updates): semantic versioning (major.minor.patch)


## test: Run all tests
test:
	go test ./...


## install: Install this application
install:
	go build -o "`go env GOPATH`/bin/example" -a -trimpath -ldflags="-s -w -X main.build=`date '+%Y%m%d%H%M%S'`" -gcflags="all=-trimpath=/src" -asmflags="all=-trimpath=/src" main.go


## cross-compile-linux: Cross-compile for Linux operating systems
cross-compile-linux:
	mkdir -p bin/linux && env GOOS=linux GOARCH=386 go build -o bin/linux/example -a -trimpath -ldflags="-s -w -X main.build=`date '+%Y%m%d%H%M%S'`" -gcflags="all=-trimpath=/src" -asmflags="all=-trimpath=/src"


## cross-compile-mac: Cross-compile for macOS operating systems
cross-compile-mac:
	mkdir -p bin/darwin && env GOOS=darwin GOARCH=386 go build -o bin/mac/example -a -trimpath -ldflags="-s -w -X main.build=`date '+%Y%m%d%H%M%S'`" -gcflags="all=-trimpath=/src" -asmflags="all=-trimpath=/src"


## cross-compile-windows: Cross-compile for Windows operating systems
cross-compile-windows:
	mkdir -p bin/windows && env GOOS=windows GOARCH=386 go build -o bin/windows/example.exe -a -trimpath -ldflags="-s -w -X main.build=`date '+%Y%m%d%H%M%S'`" -gcflags="all=-trimpath=/src" -asmflags="all=-trimpath=/src"


## clean: Clean stuff
clean:
	go clean
