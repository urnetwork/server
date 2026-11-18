package task

import "testing"

func TestTaskArgumentPrivacyCoversUnknownAndMalformedPayloads(t *testing.T) {
	for _, test := range []struct{ name, raw, want string }{
		{"private", `{"_private_task_arguments":true,"secret":"synthetic-private-value"}`, "[private task arguments]"},
		{"false marker", `{"_private_task_arguments":false,"secret":"synthetic-private-value"}`, "[private task arguments]"},
		{"null marker", `{"_private_task_arguments":null,"secret":"synthetic-private-value"}`, "[private task arguments]"},
		{"string marker", `{"_private_task_arguments":"false","secret":"synthetic-private-value"}`, "[private task arguments]"},
		{"object marker", `{"_private_task_arguments":{},"secret":"synthetic-private-value"}`, "[private task arguments]"},
		{"duplicate marker", `{"_private_task_arguments":true,"_private_task_arguments":false,"secret":"synthetic-private-value"}`, "[private task arguments]"},
		{"case variant", `{"_PRIVATE_TASK_ARGUMENTS":true,"secret":"synthetic-private-value"}`, "[private task arguments]"},
		{"case variant duplicate", `{"_PRIVATE_TASK_ARGUMENTS":true,"_private_task_arguments":false,"secret":"synthetic-private-value"}`, "[private task arguments]"},
		{"escaped marker", `{"\u005fprivate_task_arguments":false,"secret":"synthetic-private-value"}`, "[private task arguments]"},
		{"unicode fold", `{"_private_ta\u017fk_arguments":null,"secret":"synthetic-private-value"}`, "[private task arguments]"},
		{"public", ` { "shard" : 2 } `, ` { "shard" : 2 } `},
		{"nested marker", `{"metadata":{"_private_task_arguments":true},"shard":2}`, `{"metadata":{"_private_task_arguments":true},"shard":2}`},
		{"null arguments", `null`, `null`},
		{"malformed", `{"_private_task_arguments":true,"secret":`, "[invalid task arguments]"},
		{"trailing object", `{"_private_task_arguments":true} {"secret":"synthetic-private-value"}`, "[invalid task arguments]"},
		{"array", `[{"_private_task_arguments":true,"secret":"synthetic-private-value"}]`, "[invalid task arguments]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ArgumentsForLog(test.raw); got != test.want {
				t.Fatal("task argument privacy boundary changed")
			}
		})
	}
}
