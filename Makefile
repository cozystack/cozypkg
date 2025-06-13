VERSION=$(shell git describe --tags)

build:
	go build -ldflags="-X 'main.Version=$(VERSION)'"
