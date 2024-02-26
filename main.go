package main

import "github.com/restartfu/decryptmypack/app"

func main() {
	a := app.App{}
	err := a.ListenAndServe(":6969")
	if err != nil {
		panic(err)
	}
}
