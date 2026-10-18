package main

import (
	"testing"
	"time"
)

func TestParseRecognizesTokenCreditAndMCP(t *testing.T) {
	body := []byte("{\"code\":200,\"success\":true,\"data\":{\"level\":\"pro\",\"limits\":[" +
		"{\"type\":\"TIME_LIMIT\",\"unit\":5,\"number\":1,\"percentage\":0,\"nextResetTime\":1786489348996}," +
		"{\"type\":\"TOKENS_LIMIT\",\"unit\":3,\"number\":5,\"percentage\":42,\"nextResetTime\":1786493397235}," +
		"{\"type\":\"CREDIT_LIMIT\",\"unit\":3,\"number\":5,\"usage\":100,\"remaining\":75,\"percentage\":0,\"nextResetTime\":9999999999999}" +
		"]}}")
	now := time.Unix(1800000000, 0)
	plan, windows, err := parseZhipuQuota(body, now)
	if err != nil || plan != "pro" || len(windows) != 3 {
		t.Fatalf("%v %s %d", err, plan, len(windows))
	}
	if windows[0].Label != "MCP" || windows[1].Label != "额度 5小时" || windows[1].Used != 42 {
		t.Fatalf("%+v", windows[0])
	}
	if windows[2].Label != "积分 5小时" || windows[2].Used != 25 || windows[2].HasReset {
		t.Fatal(windows[2].Label, windows[2].Used, windows[2].HasReset)
	}
	if !windows[1].HasReset {
		t.Fatal("missing reset")
	}
}
