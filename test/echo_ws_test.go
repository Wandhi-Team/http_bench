package test

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"testing"

	"github.com/gorilla/websocket"
)

const (
	NAMEWS = "WS"
)

var upgrader = websocket.Upgrader{} // use default options

func TestEchoWS(t *testing.T) {
	listen := "0.0.0.0:18094"
	if len(os.Args) > 5 {
		listen = os.Args[len(os.Args)-1]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Print("upgrade:", err)
			return
		}
		defer c.Close()
		for {
			mt, message, err := c.ReadMessage()
			if err != nil {
				log.Println(NAMEWS+" read:", err)
				break
			}
			if message != nil {
				log.Println("message: ", string(message))
			}
			err = c.WriteMessage(mt, message)
			if err != nil {
				log.Println(NAMEWS+" write:", err)
				break
			}
		}
	})
	fmt.Fprintf(os.Stdout, NAMEWS+" Server listen %s\n", listen)
	if err := http.ListenAndServe(listen, mux); err != nil {
		fmt.Fprintf(os.Stderr, NAMEWS+" ListenAndServe err: %s\n", err.Error())
	}
}
