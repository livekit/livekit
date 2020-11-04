package commands

import (
	"encoding/json"
	"fmt"
)

func PrintJSON(obj interface{}) {
	txt, _ := json.Marshal(obj)
	fmt.Println(string(txt))
}
