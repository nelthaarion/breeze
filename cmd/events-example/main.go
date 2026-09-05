// Command events-example is the smallest thing that shows the event bus working:
// register listeners, emit one event, watch them run.
//
// Deliberately not a server. The bus is not an HTTP feature — workflow and
// observability both use it without importing breeze — and an example that started
// a listener on port 3000 would suggest otherwise. It runs, prints, and exits.
//
// The sleep at the end is what makes the async listener's output visible. In a real
// application the process outlives the dispatch; here it would not, and an example
// whose output depends on scheduling luck teaches the wrong thing.
//
//	cd cmd/events-example && go run .
package main

import (
	"fmt"
	"time"

	"github.com/nelthaarion/breeze/v2/events"
)

func main() {

	fmt.Println("🚀 Breeze Event Demo")

	// register handlers
	RegisterListeners()

	fmt.Println("Creating user...")

	events.Emit(
		UserRegistered{
			ID:    1,
			Email: "test@example.com",
		},
	)

	fmt.Println("Request finished")

	time.Sleep(time.Second)

}
