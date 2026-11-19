// Protocol transport is portable; native executor authority requires Linux.
package competitionagent

import "errors"

// Unsupported hosts must never acknowledge a native operation or gain authority.
var ErrUnsupportedPlatform = errors.New("subevaluation operation is unsupported on this platform")
