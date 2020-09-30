package keptnkubeutils

import (
	"testing"

	"github.com/magiconair/properties/assert"
	"helm.sh/helm/v3/pkg/chart/loader"
)

var recommendedLabelsCharts = []struct {
	chartPath      string
	expectedResult bool
}{
	{"testdata/all_recommended_labels", true},
	{"testdata/some_recommended_labels", false},
}

func TestCheckRecommendedLabels(t *testing.T) {
	for _, tt := range recommendedLabelsCharts {
		t.Run(tt.chartPath, func(t *testing.T) {
			c, err := loader.Load(tt.chartPath)
			if err != nil {
				t.Fatalf("Failed to load testdata: %s", err)
			}
			hasRecommendedLabels, err := CheckRecommendedLabels(c)
			assert.Equal(t, err, nil, "Unexpected error")
			assert.Equal(t, hasRecommendedLabels, tt.expectedResult, "Wrong check")
		})
	}
}
