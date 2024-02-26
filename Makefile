all:
	go env -w GOOS=linux
	go build .
	go env -w GOOS=windows