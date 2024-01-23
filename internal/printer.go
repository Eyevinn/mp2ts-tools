package internal

import (
	"encoding/json"
	"fmt"
	"io"
)

type JsonPrinter struct {
	W        io.Writer
	Indent   bool
	AccError error
}

func (p *JsonPrinter) Print(data any, show bool) {
	if !show {
		return
	}
	var out []byte
	var err error
	if p.AccError != nil {
		return
	}
	if p.Indent {
		out, err = json.MarshalIndent(data, "", "  ")
	} else {
		out, err = json.Marshal(data)
	}
	if err != nil {
		p.AccError = err
		return
	}
	_, p.AccError = fmt.Fprintln(p.W, string(out))
}

func (p *JsonPrinter) Error() error {
	return p.AccError
}
