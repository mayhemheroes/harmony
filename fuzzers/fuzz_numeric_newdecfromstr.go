package myfuzz

import (
	"github.com/harmony-one/harmony/numeric"
)

func Fuzz(data []byte) int {
	numeric.NewDecFromStr(string(data))
	return 1
}
