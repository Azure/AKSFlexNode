package main

//go:generate sh -c "PATH=hack/bin/bin:hack/bin:$PATH find . -name '*.proto' -not -path './hack/*' -exec protoc -I. -Ihack/bin/include --go_opt=paths=source_relative --go_out=. --go-grpc_opt=paths=source_relative --go-grpc_out=. {} +"
