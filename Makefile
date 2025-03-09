BINARY_NAME=fileserver
MAIN_FILE=main.go

.PHONY: all build clean run

all: build

build:
	@echo "Building..."
	go build -o $(BINARY_NAME) $(MAIN_FILE)
	@echo "Build complete: $(BINARY_NAME)"

clean:
	@echo "Cleaning..."
	go clean
	rm -f $(BINARY_NAME)
	rm -f $(BINARY_NAME).exe

run: build
	@echo "Starting server..."
	./$(BINARY_NAME)

install:
	@echo "Installing dependencies..."
	go mod tidy
