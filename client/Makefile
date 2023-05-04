



all: clean build_android build_ios

clean:
	rm -rf build

build_android:
	mkdir -p build/android
	gomobile bind -target=android -androidapi 19 -javapkg com.bringyour.network -o build/android/client.aar

build_ios:
	mkdir -p build/ios
	# -prefix com.bringyour.network.client
	gomobile bind -target=ios -iosversion 15.0 -o build/ios/Client.xcframework

init:
	go install golang.org/x/mobile/cmd/gomobile@latest
	gomobile init
