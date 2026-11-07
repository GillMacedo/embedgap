.PHONY: test lint demo

test:
	test -z "$$(gofmt -l plugin examples tests)"
	go vet ./...
	go test -race ./...

lint:
	claude plugin validate ./plugin --strict
	claude plugin validate . --strict

# Exit 1 means it found problems, which the broken example has on purpose.
demo:
	go run ./plugin/scripts --dir examples/brokenembed || test $$? -eq 1
	go run ./plugin/scripts --dir examples/fixedembed
