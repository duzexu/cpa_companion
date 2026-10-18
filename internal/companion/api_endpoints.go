package companion

// apiEndpoints is also used by the key page so its route list stays in sync
// with the paths accepted by the proxy. Every POST route supports metering;
// GET /v1/models needs no usage metering.
type apiEndpoint struct {
	Method string
	Path   string
}

var apiEndpoints = []apiEndpoint{
	{Method: "GET", Path: "/v1/models"},
	{Method: "POST", Path: "/v1/chat/completions"},
	{Method: "POST", Path: "/v1/completions"},
	{Method: "POST", Path: "/v1/responses"},
	{Method: "POST", Path: "/v1/responses/compact"},
	{Method: "POST", Path: "/v1/messages"},
	{Method: "POST", Path: "/v1/images/generations"},
	{Method: "POST", Path: "/v1/alpha/search"},
}

func forwardJSONRoute(route string) bool {
	for _, endpoint := range apiEndpoints {
		if endpoint.Method == "POST" && endpoint.Path == route {
			return true
		}
	}
	return false
}

func meterableRoute(route string) bool {
	return forwardJSONRoute(route)
}
