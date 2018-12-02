test:
	go test -v -coverprofile=cover.out -timeout 30s
cover:
	go tool cover -html=cover.out
vet:
	go vet github.com/ajbeach2/worker

bench:
	go test -v -coverprofile=cover.out -cpuprofile cpu.prof -memprofile mem.prof -bench .

