.PHONY: build run clean test

build:
	go mod tidy
	go build -o bin/router cmd/router/main.go

run: build
	./bin/router

clean:
	rm -rf bin/

test:
	go test -v ./...

install: build
	sudo cp bin/router /usr/local/bin/s2-router
	sudo chmod +x /usr/local/bin/s2-router
