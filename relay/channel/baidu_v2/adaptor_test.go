package baidu_v2

import (
	"testing"

	relaycommon "github.com/dengyie/apihub/relay/common"
	relayconstant "github.com/dengyie/apihub/relay/constant"
)

func TestGetRequestURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		baseURL    string
		relayMode  int
		want       string
		wantErrMsg bool
	}{
		{
			name:      "canonical base uses v2 prefix",
			baseURL:   "https://qianfan.baidubce.com",
			relayMode: relayconstant.RelayModeChatCompletions,
			want:      "https://qianfan.baidubce.com/v2/chat/completions",
		},
		{
			name:      "token plan personal base skips v2 prefix",
			baseURL:   "qianfan-tokenplan-personal",
			relayMode: relayconstant.RelayModeChatCompletions,
			want:      "https://qianfan.baidubce.com/v2/tokenplan/personal/chat/completions",
		},
		{
			name:      "token plan personal embeddings",
			baseURL:   "qianfan-tokenplan-personal",
			relayMode: relayconstant.RelayModeEmbeddings,
			want:      "https://qianfan.baidubce.com/v2/tokenplan/personal/embeddings",
		},
		{
			name:       "token plan personal rejects unsupported mode",
			baseURL:    "qianfan-tokenplan-personal",
			relayMode:  relayconstant.RelayModeRerank,
			wantErrMsg: true,
		},
	}

	for _, tc := range cases {
		info := &relaycommon.RelayInfo{
			RelayMode: tc.relayMode,
			ChannelMeta: &relaycommon.ChannelMeta{
				ChannelBaseUrl: tc.baseURL,
			},
		}
		got, err := (&Adaptor{}).GetRequestURL(info)
		if tc.wantErrMsg {
			if err == nil {
				t.Fatalf("%s: expected error, got url %q", tc.name, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
