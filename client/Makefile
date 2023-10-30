



all: clean build_android build_ios

clean:
	rm -rf build

build_android:
	mkdir -p build/android
# 	gomobile bind -target=android -androidapi 19 -javapkg com.bringyour.network -o build/android/client.aar
	WARP_VERSION=`warpctl ls version`; \
	gomobile bind \
		-ldflags "-X client.Version=$$WARP_VERSION" \
		-target=android -androidapi 19 \
		-javapkg com.bringyour.network.goclient \
		-o build/android/goclient.aar \
	bringyour.com/client

build_ios:
	mkdir -p build/ios
	# -prefix com.bringyour.network.client
# 	gomobile bind -target=ios -iosversion 14.0 -o build/ios/Client.xcframework
	gomobile bind -target=ios -iosversion 14.0 -o build/ios/Client.xcframework bringyour.com/client bringyour.com/client/device bringyour.com/client/vc

init:
	go install golang.org/x/mobile/cmd/gomobile@latest
	gomobile init
