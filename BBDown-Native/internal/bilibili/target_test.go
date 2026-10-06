package bilibili

import (
	"strconv"
	"strings"
	"testing"
)

// fmtTargets 把结果压成一行便于断言，例如 "BV1xx411c7mD@2,av170001"。
func fmtTargets(ts []Target) string {
	parts := make([]string, 0, len(ts))
	for _, t := range ts {
		s := t.ID
		if t.Short {
			s = "short"
		}
		if s == "" {
			s = "?"
		}
		if t.Page > 0 {
			s += "@" + strconv.Itoa(t.Page)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ",")
}

func TestExtractTargets(t *testing.T) {
	const (
		bvA = "BV1xx411c7mD"
		bvB = "BV1yy411c7mD"
	)

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			// 这是用户实际反馈的场景：整段分享文案。
			name: "分享文案_标题加链接加追踪参数",
			in:   "【Hsin let's Groove!】 https://www.bilibili.com/video/BV12eHa6RE59/?share_source=copy_web&vd_source=948b033cbf8aaa1bf41502231246ba13",
			want: "BV12eHa6RE59",
		},
		{
			name: "链接带分P参数",
			in:   "https://www.bilibili.com/video/" + bvA + "?p=2",
			want: bvA + "@2",
		},
		{
			name: "一段话里混合BV号和av号",
			in:   "安利一下 " + bvA + " 还有 av170001 都不错",
			want: bvA + ",av170001",
		},
		{
			name: "短链与普通链接并存",
			in:   "https://b23.tv/abc123 还有 https://www.bilibili.com/video/" + bvB,
			want: "short," + bvB,
		},
		{
			name: "无协议写法也要补全并保留分P",
			in:   "www.bilibili.com/video/" + bvA + "?p=3",
			want: bvA + "@3",
		},
		{
			name: "非B站链接必须忽略",
			in:   "https://www.youtube.com/watch?v=" + bvA,
			want: "",
		},
		{
			name: "中文标点包裹",
			in:   "（https://www.bilibili.com/video/" + bvA + "），很好看",
			want: bvA,
		},
		{
			name: "emoji紧贴链接",
			in:   "🤔https://www.bilibili.com/video/" + bvA + "🤔",
			want: bvA,
		},
		{
			name: "Markdown链接",
			in:   "[标题](https://www.bilibili.com/video/" + bvA + ")",
			want: bvA,
		},
		{
			name: "番剧链接",
			in:   "https://www.bilibili.com/bangumi/play/ep123456",
			want: "ep123456",
		},
		{
			name: "重复出现要去重",
			in:   bvA + " " + bvA + " av0170001 av170001",
			want: bvA + ",av170001",
		},
		{
			name: "裸ep与ss号",
			in:   "ep123456 与 ss98765",
			want: "ep123456,ss98765",
		},
		{
			name: "大小写前缀要归一",
			in:   "bv1xx411c7mD",
			want: bvA,
		},
		{
			name: "被长串包住的不算",
			in:   "xav123 " + bvA + "extra",
			want: "",
		},
		{
			name: "多行多链接保序",
			in:   "第一集 https://www.bilibili.com/video/BV1aa411c7mD?p=1\n第二集 https://www.bilibili.com/video/BV1bb411c7mD?p=2",
			want: "BV1aa411c7mD@1,BV1bb411c7mD@2",
		},
		{
			name: "空白输入",
			in:   "   \n  ",
			want: "",
		},
		{
			name: "纯文案无地址",
			in:   "这个视频真好看，但是我没贴链接",
			want: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fmtTargets(ExtractTargets(c.in))
			if got != c.want {
				t.Errorf("\n输入: %q\n期望: %q\n实际: %q", c.in, c.want, got)
			}
		})
	}
}

func TestSanitizeFileName(t *testing.T) {
	cases := []struct{ in, want string }{
		{`a/b\c:d*e?f"g<h>i|j`, "a_b_c_d_e_f_g_h_i_j"},
		{"结尾有点 . ", "结尾有点"},
		{"", "未命名"},
		{"正常标题【高画质】", "正常标题【高画质】"},
	}
	for _, c := range cases {
		if got := SanitizeFileName(c.in); got != c.want {
			t.Errorf("SanitizeFileName(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

func TestPickPages(t *testing.T) {
	v := &VideoInfo{Pages: []Page{
		{Index: 1, Cid: 101}, {Index: 2, Cid: 102}, {Index: 3, Cid: 103},
		{Index: 4, Cid: 104}, {Index: 5, Cid: 105},
	}}
	got, err := v.PickPages("2,4-5")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Index != 2 || got[1].Index != 4 || got[2].Index != 5 {
		t.Fatalf("PickPages 结果不对: %+v", got)
	}
	if all, _ := v.PickPages(""); len(all) != 5 {
		t.Fatalf("空选择应返回全部分P，得到 %d 个", len(all))
	}
	if _, err := v.PickPages("9"); err == nil {
		t.Fatal("选中不存在的分P应当报错")
	}
	if _, err := v.PickPages("5-2"); err == nil {
		t.Fatal("倒序范围应当报错")
	}
}
