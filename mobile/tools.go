//go:build tools

package mobile

// gomobile needs golang.org/x/mobile in go.mod to build this package.
import _ "golang.org/x/mobile/bind"
