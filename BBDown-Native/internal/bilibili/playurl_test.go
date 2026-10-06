package bilibili

import "testing"

func mv(id int, codecs string, bw int) Stream {
	return Stream{ID: id, Codecs: codecs, Bandwidth: bw}
}

func TestSelectVideoPrefersAVC(t *testing.T) {
	// 同一画质下同时存在 HEVC 与 AVC —— 必须选 AVC。
	// 现实后果：选HEVC 会让产物在哔哩哔哩客户端里全黑。
	p := &PlayURL{Video: []Stream{
		mv(80, "HEVC", 150000),
		mv(80, "AVC", 300000),
		mv(80, "AV1", 120000),
	}}
	s, err := p.SelectVideo(nil, nil)
	if err != nil {
		t.Fatalf("SelectVideo: %v", err)
	}
	if got := CodecName(s.Codecs); got != "AVC" {
		t.Fatalf("默认应选 AVC，实际选了 %s（HEVC 优先会让部分播放器全黑）", got)
	}
}

func TestSelectVideoRespectsExplicitCodec(t *testing.T) {
	p := &PlayURL{Video: []Stream{
		mv(80, "HEVC", 150000),
		mv(80, "AVC", 300000),
	}}
	// 用户在界面显式选了 HEVC —— 这是明确偏好，必须尊重（哪怕体积小、兼容性差）。
	s, err := p.SelectVideo(nil, []string{"hevc"})
	if err != nil {
		t.Fatalf("SelectVideo: %v", err)
	}
	if got := CodecName(s.Codecs); got != "HEVC" {
		t.Fatalf("显式指定 hevc 时应选 HEVC，实际 %s", got)
	}
}

func TestSelectVideoNoAVCFallsBack(t *testing.T) {
	// 该画质下只有 HEVC（不少站的 1080P+ 是 HEVC 独占）—— 不能因此失败，
	// 要能顺位退到 HEVC，否则等于「高画质下不了」。
	//注意 80 画质那档是有 AVC 的，用来确认它不是被全局偏好带偏的。
	p := &PlayURL{Video: []Stream{
		mv(80, "AVC", 300000),
		mv(116, "HEVC", 900000),
	}}
	s, err := p.SelectVideo([]int{116, 80}, nil)
	if err != nil {
		t.Fatalf("高画质缺 AVC 时不该报错: %v", err)
	}
	if got := CodecName(s.Codecs); got != "HEVC" {
		t.Fatalf("116 画质只有 HEVC 时应回退到 HEVC，实际 %s", got)
	}
}

func TestSelectVideoQualityPriorityBeatsCodec(t *testing.T) {
	// 画质优先于编码：低画质的 AVC 不该抢走高画质的 HEVC。
	p := &PlayURL{Video: []Stream{
		mv(64, "AVC", 800000),
		mv(80, "HEVC", 150000),
	}}
	s, err := p.SelectVideo([]int{80, 64}, nil)
	if err != nil {
		t.Fatalf("SelectVideo: %v", err)
	}
	if s.ID != 80 {
		t.Fatalf("应优先取画质 80，实际 %d", s.ID)
	}
}