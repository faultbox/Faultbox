.PHONY: build test clean lint fmt vet run

APP_NAME := faultbox
BIN_DIR  := bin

build:
	go build -o $(BIN_DIR)/$(APP_NAME) ./cmd/faultbox

run: build
	./$(BIN_DIR)/$(APP_NAME)

test:
	go test ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

lint: fmt vet

clean:
	rm -rf $(BIN_DIR)
