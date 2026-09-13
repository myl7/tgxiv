.PHONY: build test clean

build:
	go build -o bin/tgxiv .

test:
	go test ./...

clean:
	rm -rf bin/
