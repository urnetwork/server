
all: clean build

clean:
	rm -rf build

# the linux hosts operators run it on, and the macOS builder, whose certs-renew.sh builds its own
build:
	env CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o build/linux/arm64/
	env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o build/linux/amd64/
	env CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o build/darwin/arm64/
	env CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -o build/darwin/amd64/
