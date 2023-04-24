

all: build image

build:
	# https://www.digitalocean.com/community/tutorials/how-to-build-go-executables-for-multiple-platforms-on-ubuntu-16-04
	env GOOS=linux GOARCH=arm64 go build -o bin/linux/arm64/
	env GOOS=linux GOARCH=amd64 go build -o bin/linux/amd64/
	env GOOS=darwin GOARCH=arm64 go build -o bin/darwin/arm64/
	env GOOS=darwin GOARCH=amd64 go build -o bin/darwin/amd64/

image:
	docker buildx build --platform linux/arm64/v8,linux/amd64 . -t bringyour/canary-api:2023.01.1 --no-cache --push


#run:
	# docker rm api-beta
	# docker run --name api-beta -p 7481:80 -e MESSAGE=api-2023.01.1-beta bringyour/canary-api:2023.01.1
	# api-g1
	# api-g2
	# api-g3
	# api-g4

# sudo docker run -p 7481:80 --network warpsbs -e MESSAGE=api-2023.01.1-beta bringyour/canary-api:2023.01.1

# docker run --mount type=bind,source=/Users/brien/bringyour,target=/srv/bringyour,readonly bringyour/canary-api:2023.01.1

