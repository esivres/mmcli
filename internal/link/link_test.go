package link

import "testing"

func TestParse(t *testing.T) {
	const id = "abcdefghijklmnopqrstuvwxyz" // 26 chars
	cases := []struct {
		name    string
		in      string
		want    Ref
		wantErr bool
	}{
		{"bare id", id, Ref{PostID: id}, false},
		{"permalink", "https://mm.example.com/myteam/pl/" + id, Ref{Team: "myteam", PostID: id}, false},
		{"channel url", "https://mm.example.com/myteam/channels/town-square", Ref{Team: "myteam", ChannelName: "town-square"}, false},
		{"empty", "", Ref{}, true},
		{"garbage", "not a link", Ref{}, true},
		{"unknown path", "https://mm.example.com/myteam/something/x", Ref{}, true},
		{"bad permalink id", "https://mm.example.com/myteam/pl/short", Ref{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %+v", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Parse(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}
