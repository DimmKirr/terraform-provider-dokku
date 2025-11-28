package dokkuclient

import (
	"context"
	"fmt"
	"strings"
)

// BuilderSet sets a builder property for an app
// property can be: "selected", "build-dir"
// Pass empty value to clear a property
func (c *Client) BuilderSet(ctx context.Context, appName string, property string, value string) error {
	_, _, err := c.RunQuiet(ctx, fmt.Sprintf("builder:set %s %s %s", appName, property, value))
	return err
}

// BuilderReport gets builder configuration for an app
// Returns a map with keys like:
// - "Builder build dir"
// - "Builder computed build dir"
// - "Builder selected"
// - "Builder computed selected"
func (c *Client) BuilderReport(ctx context.Context, appName string) (map[string]string, error) {
	stdout, _, err := c.RunQuiet(ctx, fmt.Sprintf("builder:report %s", appName))
	if err != nil {
		return nil, err
	}

	res := make(map[string]string)
	lines := strings.Split(stdout, "\n")
	for _, line := range lines {
		// Parse lines like "Builder build dir:    app2"
		index := strings.Index(line, ":")
		if index == -1 {
			continue
		}
		key := strings.TrimSpace(line[:index])
		value := strings.TrimSpace(line[index+1:])
		res[key] = value
	}
	return res, nil
}
