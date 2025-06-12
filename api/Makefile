
all: clean build


clean:
	rm -rf build

build:
	# generate the api docs into the latest build
	npx -y @redocly/cli build-docs bringyour.yml -o build/api.html
