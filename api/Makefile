

all: build image

build:
	# https://www.digitalocean.com/community/tutorials/how-to-build-go-executables-for-multiple-platforms-on-ubuntu-16-04
	env GOOS=linux GOARCH=arm64 go build -o bin/linux/arm64/
	env GOOS=linux GOARCH=amd64 go build -o bin/linux/amd64/
	env GOOS=darwin GOARCH=arm64 go build -o bin/darwin/arm64/
	env GOOS=darwin GOARCH=amd64 go build -o bin/darwin/amd64/

image:
	docker buildx build --platform linux/arm64/v8,linux/amd64 . -t bringyour/canary-api:2023.01.1 --no-cache --push


# docker run --name api-beta -p 7481:80 --network warpsbs bringyour/canary-api:2023.01.1

#run:
	# docker rm api-beta
	# docker run --name api-beta -p 7481:80 -e MESSAGE=api-2023.01.1-beta bringyour/canary-api:2023.01.1
	# api-g1
	# api-g2
	# api-g3
	# api-g4


# sudo docker run --name api-beta -p 7481:80 --network warpsbs --add-host warpsbs:172.18.0.1 --mount type=bind,source=/srv/bringyour,target=/srv/bringyour,readonly --mount type=bind,source=/srv/bringyour/keys-red/aws,target=/root/.aws,readonly bringyour/canary-api:2023.01.1
# sudo docker run --name api-g1 -p 7501:80 --network warpsbs --add-host warpsbs:172.18.0.1 --mount type=bind,source=/srv/bringyour,target=/srv/bringyour,readonly --mount type=bind,source=/srv/bringyour/keys-red/aws,target=/root/.aws,readonly bringyour/canary-api:2023.01.1
# sudo docker run --name api-g2 -p 7521:80 --network warpsbs --add-host warpsbs:172.18.0.1 --mount type=bind,source=/srv/bringyour,target=/srv/bringyour,readonly --mount type=bind,source=/srv/bringyour/keys-red/aws,target=/root/.aws,readonly bringyour/canary-api:2023.01.1
# sudo docker run --name api-g3 -p 7541:80 --network warpsbs --add-host warpsbs:172.18.0.1 --mount type=bind,source=/srv/bringyour,target=/srv/bringyour,readonly --mount type=bind,source=/srv/bringyour/keys-red/aws,target=/root/.aws,readonly bringyour/canary-api:2023.01.1
# sudo docker run --name api-g4 -p 7561:80 --network warpsbs --add-host warpsbs:172.18.0.1 --mount type=bind,source=/srv/bringyour,target=/srv/bringyour,readonly --mount type=bind,source=/srv/bringyour/keys-red/aws,target=/root/.aws,readonly bringyour/canary-api:2023.01.1
