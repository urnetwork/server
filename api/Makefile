
build:
	cp -r ~/bringyour/xops/echo/bin ./
	docker buildx build --platform linux/arm64/v8,linux/amd64 . -t bringyour/canary-api:2023.01.1 --no-cache --push

run:
	# docker rm api-beta
	# docker run --name api-beta -p 7481:80 -e MESSAGE=api-2023.01.1-beta bringyour/canary-api:2023.01.1
	# api-g1
	# api-g2
	# api-g3
	# api-g4

# sudo docker run -p 7481:80 --network warpsbs -e MESSAGE=api-2023.01.1-beta bringyour/canary-api:2023.01.1