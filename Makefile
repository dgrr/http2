.PHONY: test

test:
	go test -v
	cd h2spec && go test -v
