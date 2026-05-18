.PHONY: build run clean lint

build:
	go build -o telegram-translator .

run: build
	./telegram-translator

clean:
	rm -f telegram-translator

lint:
	go vet ./...
	go fmt ./...
