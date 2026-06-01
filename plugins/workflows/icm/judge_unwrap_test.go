package icm

import "testing"

func TestUnwrapJudgeJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "bare json passes through",
			in:   `{"verdict":"pass","feedback":"ok"}`,
			want: `{"verdict":"pass","feedback":"ok"}`,
		},
		{
			name: "json fence with language",
			in:   "```json\n{\"verdict\":\"fail\",\"feedback\":\"x\"}\n```",
			want: `{"verdict":"fail","feedback":"x"}`,
		},
		{
			name: "bare fence",
			in:   "```\n{\"verdict\":\"pass\"}\n```",
			want: `{"verdict":"pass"}`,
		},
		{
			name: "preamble before json",
			in:   "Here is my verdict:\n{\"verdict\":\"pass\",\"feedback\":\"good\"}",
			want: `{"verdict":"pass","feedback":"good"}`,
		},
		{
			name: "preamble + trailing prose",
			in:   "Verdict:\n{\"verdict\":\"fail\",\"feedback\":\"bad\"}\nLet me know if you need more.",
			want: `{"verdict":"fail","feedback":"bad"}`,
		},
		{
			name: "whitespace-only input",
			in:   "   \n\t",
			want: "",
		},
		{
			name: "no braces returns as-is",
			in:   "Verdict: pass",
			want: "Verdict: pass",
		},
		{
			name: "fence with uppercase lang",
			in:   "```JSON\n{\"verdict\":\"pass\"}\n```",
			want: `{"verdict":"pass"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unwrapJudgeJSON(tc.in)
			if got != tc.want {
				t.Errorf("unwrapJudgeJSON(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
