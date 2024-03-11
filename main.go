package main

import (
	"fmt"
	"github.com/restartfu/decryptmypack/app"
	"net/http"
)

func main() {
	go func() {
		err := http.ListenAndServe(":80", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Println("Redirecting to https")
			http.Redirect(w, r, "https://decryptmypack.com", http.StatusMovedPermanently)
		}))
		if err != nil {
			panic(err)
		}
	}()

	a := app.App{}
	err := a.ListenAndServe(":443")
	if err != nil {
		panic(err)
	}
}
