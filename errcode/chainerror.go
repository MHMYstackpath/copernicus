package errcode

import "fmt"

type ChainErr int

const (
	ErrorBlockHeaderNoValid ChainErr = ChainErrorBase + iota
	ErrorBlockHeaderNoParent
)

var ChainErrString = map[ChainErr]string {
	ErrorBlockHeaderNoValid: "The block header is not valid",
	ErrorBlockHeaderNoParent: "Can not find this block header's father ",
}

func (chainerr ChainErr) String() string {
	if s, ok := ChainErrString[chainerr]; ok {
		return s
	}
	return fmt.Sprintf("Unknown code (%d)",chainerr)
}