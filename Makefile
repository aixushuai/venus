all:
	go run ./build/*.go build

deps:
	git submodule update --init
	go run ./build/*.go smartdeps

# WARNING THIS BUILDS A GO PLUGIN AND PLUGINS *DO NOT* WORK ON WINDOWS SYSTEMS
iptb:
	make -C tools/iptb-plugins all

clean:
	rm ./venus
	rm -rf ./vendors/fil-blst
	rm -rf ./vendors/filecoin-ffi
	rm -rf ./vendors/test-vectors