package main

//go:generate sh -c "find . -name '*.proto' -exec protoc --go_opt=paths=source_relative --go_out=. {} +"
