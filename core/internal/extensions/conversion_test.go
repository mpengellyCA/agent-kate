package extensions

import "testing"

func TestPreviewConversionIsComponentLevelAndNonPromissory(t *testing.T) {
	preview := PreviewConversion(Extension{Name: "bundle", Harness: "claude", Components: []Component{
		{Kind: KindSkill, Name: "review"}, {Kind: KindMCP, Name: "tracker"}, {Kind: KindHook, Name: "guard"},
	}}, "codex")
	if len(preview.Components) != 3 {
		t.Fatalf("components = %#v", preview.Components)
	}
	if preview.Components[0].Status != ConversionPortable || preview.Components[1].Status != ConversionReview || preview.Components[2].Status != ConversionBlocked {
		t.Fatalf("conversion statuses = %#v", preview.Components)
	}
}
