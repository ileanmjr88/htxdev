package core

import "testing"

func TestFoldName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Houston Linux User’s Group", "houston linux user's group"},
		{"Houston Linux User's Group", "houston linux user's group"},
		{"  HOUSTON   linux  ", "houston linux"},
		{"Ion – Conference Room 030", "ion - conference room 030"},
		{"Ion — Lobby", "ion - lobby"},
		{"“Quoted”", "\"quoted\""},
		{"non breaking", "non breaking"},
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range cases {
		if got := FoldName(tc.in); got != tc.want {
			t.Errorf("FoldName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
