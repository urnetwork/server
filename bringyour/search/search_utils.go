package search


import (
    "strings"
    "regexp"

    "github.com/mozillazg/go-unidecode"
)



// a standard normalization
func NormalizeForSearch(value string) string {
    norm := strings.TrimSpace(value)
    // convert unicode chars to their latin1 equivalents
    norm = unidecode.Unidecode(value)
    norm = strings.ToLower(norm)
    // replace whitespace with a single space
    re := regexp.MustCompile("\\s+")
    norm = re.ReplaceAllString(norm, " ")
    return norm
}
