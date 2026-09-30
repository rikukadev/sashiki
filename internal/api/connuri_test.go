package api

import "testing"

// connURI は Port / Host / User と同じ 3 つ組を URI にしたもの(#379)。
// proxy 有効時の user は `dev@<branch>` で `@` を含むため、素朴に連結すると
// `@` が 2 個になりホストを取り違える。エスケープされていることを確かめる。
func TestConnURI(t *testing.T) {
	tests := []struct {
		name       string
		engine     string
		user       string
		domain     string
		proxyPort  int
		branch     string
		enginePort int
		want       string
	}{
		{
			name:       "proxy 有効: user の @ がエスケープされる",
			engine:     "mysql",
			user:       "dev",
			domain:     "sashiki.local",
			proxyPort:  3306,
			branch:     "pr-42",
			enginePort: 3401,
			want:       "mysql://dev%40pr-42@sashiki.local:3306/",
		},
		{
			name:       "proxy 無効: 直結。user は dev のまま、port は engine のもの",
			engine:     "mysql",
			user:       "dev",
			domain:     "sashiki.local",
			proxyPort:  0,
			branch:     "pr-42",
			enginePort: 3401,
			want:       "mysql://dev@sashiki.local:3401/",
		},
		{
			name:       "postgres は scheme が変わる",
			engine:     "postgres",
			user:       "dev",
			domain:     "sashiki.local",
			proxyPort:  5432,
			branch:     "pr-1",
			enginePort: 5401,
			want:       "postgres://dev%40pr-1@sashiki.local:5432/",
		},
		{
			name:       "IPv6 の host は角括弧で囲まれる",
			engine:     "mysql",
			user:       "dev",
			domain:     "::1",
			proxyPort:  3306,
			branch:     "pr-7",
			enginePort: 3401,
			want:       "mysql://dev%40pr-7@[::1]:3306/",
		},
		{
			name:       "user 自体に記号が入っても壊れない",
			engine:     "mysql",
			user:       "dev:user",
			domain:     "sashiki.local",
			proxyPort:  3306,
			branch:     "pr-9",
			enginePort: 3401,
			want:       "mysql://dev%3Auser%40pr-9@sashiki.local:3306/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{
				engine:    tt.engine,
				user:      tt.user,
				domain:    tt.domain,
				proxyPort: tt.proxyPort,
			}
			if got := s.connURI(tt.branch, tt.enginePort); got != tt.want {
				t.Errorf("connURI() = %q, want %q", got, tt.want)
			}
		})
	}
}

// URI にパスワードを載せない(ログ・画面・ポータルに出る値なので)。
func TestConnURIHasNoPassword(t *testing.T) {
	s := &Server{
		engine:    "mysql",
		user:      "dev",
		pass:      "s3cret",
		domain:    "sashiki.local",
		proxyPort: 3306,
	}
	got := s.connURI("pr-42", 3401)
	if want := "mysql://dev%40pr-42@sashiki.local:3306/"; got != want {
		t.Errorf("connURI() = %q, want %q", got, want)
	}
}
