package main

import (
	"github.com/sashabaranov/go-openai"
	"gopkg.in/yaml.v3"
	"testing"
)

func Test_logToFile(t *testing.T) {
	s := `{"tableName":"销售","description":"地区销售分析的数据是否准确？","columns":[]}`
	callers := []openai.ToolCall{
		{
			Function: openai.FunctionCall{
				Arguments: s,
			},
		},
	}
	out := formatCall(callers)
	t.Log(out)
	buf, _ := yaml.Marshal(out)
	t.Log(string(buf))

}
