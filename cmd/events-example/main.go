package main

import (
	"fmt"
	"time"

	"github.com/nelthaarion/breeze/events"
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
