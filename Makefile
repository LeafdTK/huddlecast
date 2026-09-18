.PHONY: build run test fmt vet mediamtx testsrc probe docker

build:
	go build -o bin/huddlecast ./cmd/huddlecast

run: build
	./bin/huddlecast serve --config huddlecast.yml

test:
	go test ./...

fmt:
	gofmt -w ./cmd ./internal

vet:
	go vet ./...

mediamtx:
	docker run --rm -it -p 8889:8889 -p 8189:8189/udp -p 1935:1935 -p 9997:9997 \
	  -v $(PWD)/deploy/mediamtx.local.yml:/mediamtx.yml:ro -v $(PWD)/recordings:/recordings \
	  bluenviron/mediamtx:1.21.0-ffmpeg

testsrc:
	./scripts/testsrc.sh $(KEY)

probe: build
	./bin/huddlecast probe --config huddlecast.yml --channel $(CHANNEL) --account $(ACCOUNT)

docker:
	docker compose -f deploy/docker-compose.yml build
