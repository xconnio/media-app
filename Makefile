test:
	go test -count=1 ./... -v

build:
	CGO_ENABLED=0 go build -o media-app .

run:
	go run .
