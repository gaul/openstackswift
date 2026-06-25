package middleware

import (
	"net/url"

	"github.com/labstack/echo/v4"
)

// DecodePathParams percent-decodes the captured path parameters (container and
// object names) when Echo routed on the escaped URL.
//
// Echo matches routes against http.Request.URL.RawPath whenever a client's
// percent-encoding differs from Go's canonical form, and it never unescapes the
// captured parameter values.  Swift names may contain characters that clients
// escape more aggressively than Go does (e.g. '!', '(', '#', '%'), so without
// this the raw "%XX" sequences would be stored and looked up verbatim.  When
// RawPath is empty the parameters already come from the decoded URL.Path and
// must be left untouched to avoid double-decoding names that legitimately
// contain a "%XX"-looking sequence.
func DecodePathParams() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if c.Request().URL.RawPath != "" {
				values := c.ParamValues()
				decoded := make([]string, len(values))
				for i, v := range values {
					if u, err := url.PathUnescape(v); err == nil {
						decoded[i] = u
					} else {
						decoded[i] = v
					}
				}
				c.SetParamValues(decoded...)
			}
			return next(c)
		}
	}
}
