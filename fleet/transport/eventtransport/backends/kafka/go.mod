module github.com/nelthaarion/breeze/v2/fleet/transport/eventtransport/backends/kafka

go 1.27.4

require (
	github.com/nelthaarion/go-json-plus v0.12.0
	github.com/nelthaarion/breeze/v2 v2.3.1
	github.com/segmentio/kafka-go v0.4.49
)

require (
	github.com/klauspost/compress v1.15.9 // indirect
	github.com/pierrec/lz4/v4 v4.1.15 // indirect
)

replace github.com/nelthaarion/breeze/v2 => ../../../../../
