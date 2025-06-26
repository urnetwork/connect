

all: init clean build

clean:
	rm -rf build

build:
	env CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-w -s -X main.Version=${WARP_VERSION}" -o build/linux/arm64/
	env CGO_ENABLED=0 GOOS=linux GOARCH=arm go build -trimpath -ldflags "-w -s -X main.Version=${WARP_VERSION}" -o build/linux/arm/
	env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-w -s -X main.Version=${WARP_VERSION}" -o build/linux/amd64/
	env CGO_ENABLED=0 GOOS=linux GOARCH=386 go build -trimpath -ldflags "-w -s -X main.Version=${WARP_VERSION}" -o build/linux/386/
	env CGO_ENABLED=0 GOOS=linux GOARCH=mips GOMIPS=softfloat go build -trimpath -ldflags "-w -s -X main.Version=${WARP_VERSION}" -o build/linux/mips/
	env CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build -trimpath -ldflags "-w -s -w -s -X main.Version=${WARP_VERSION}" -o build/linux/mipsle/
	env CGO_ENABLED=0 GOOS=linux GOARCH=mips64 GOMIPS=softfloat go build -trimpath -ldflags "-w -s -X main.Version=${WARP_VERSION}" -o build/linux/mips64/
	env CGO_ENABLED=0 GOOS=linux GOARCH=mips64le GOMIPS=softfloat go build -trimpath -ldflags "-w -s -w -s -X main.Version=${WARP_VERSION}" -o build/linux/mips64le/
	env CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-w -s -X main.Version=${WARP_VERSION}" -o build/darwin/arm64/
	env CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "-w -s -X main.Version=${WARP_VERSION}" -o build/darwin/amd64/
	env CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -trimpath -ldflags "-w -s -X main.Version=${WARP_VERSION}" -o build/windows/arm64/
	env CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-w -s -X main.Version=${WARP_VERSION}" -o build/windows/amd64/
	tar -czf build/provider.tar.gz -C build linux darwin windows

warp_build:
	$(MAKE) all
	docker buildx build --progress=plain \
		--build-arg warp_env=${WARP_ENV} \
		--platform linux/arm64/v8,linux/amd64 \
		-t ${WARP_DOCKER_NAMESPACE}/${WARP_DOCKER_IMAGE}:${WARP_DOCKER_VERSION} \
		--no-cache \
		--push \
		.

init:
	go clean -cache
	go clean -modcache
