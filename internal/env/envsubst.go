package env

// envsubst.go — the bare `envsubst` stage of the bash pipeline. The yq-shaped
// spec reads live in internal/yqsem (this package reads through the
// literal-false alternative flavour, and enabled/build through `tostring` —
// the only reliable missing-vs-false distinction). Mapping order is
// preserved (services iterate in document order, exactly like
// `yq '.services | keys'`).

import (
	"os"
)

// BareEnvsubst is GNU gettext envsubst with no SHELL-FORMAT (the bash
// pipeline's bare `envsubst`): every `${NAME}` and identifier-boundary bare
// `$NAME` reference is replaced with the variable's value — undefined vars
// become the empty string — while anything that is not a plain reference
// (`${X:-y}`, `${arr[0]}`, `$$`) passes through untouched. Single pass;
// substituted values are not rescanned.
func BareEnvsubst(data []byte) []byte {
	var out []byte
	for i := 0; i < len(data); {
		ch := data[i]
		if ch != '$' || i+1 >= len(data) {
			out = append(out, ch)
			i++
			continue
		}
		if data[i+1] == '{' {
			j := i + 2
			for j < len(data) && isIdentByte(data[j]) {
				j++
			}
			if j > i+2 && j < len(data) && data[j] == '}' {
				out = append(out, os.Getenv(string(data[i+2:j]))...)
				i = j + 1
				continue
			}
			out = append(out, ch)
			i++
			continue
		}
		if isIdentStartByte(data[i+1]) {
			j := i + 1
			for j < len(data) && isIdentByte(data[j]) {
				j++
			}
			out = append(out, os.Getenv(string(data[i+1:j]))...)
			i = j
			continue
		}
		out = append(out, ch)
		i++
	}
	return out
}

func isIdentStartByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentByte(c byte) bool {
	return isIdentStartByte(c) || (c >= '0' && c <= '9')
}
