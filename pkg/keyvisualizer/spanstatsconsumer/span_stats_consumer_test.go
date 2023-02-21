package spanstatsconsumer

import (
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/stretchr/testify/require"
	"testing"
)

func makeBoundaries(n int) []roachpb.Span {
	boundaries := make([]roachpb.Span, n)
	for i := 0; i < n; i++ {
		sp := roachpb.Span{
			Key:    roachpb.Key{byte(i)},
			EndKey: roachpb.Key{byte(i + 1)},
		}
		boundaries[i] = sp
	}
	return boundaries
}

//

func TestMaybeCombineBoundaries(t *testing.T) {

	// cases to test:
	// 1) a boundaries slice with length <= max does not get modified
	// 2) a boundaries slice with length that is not an even multiple of max
	// 3) a boundaries slice with length that is an even multiple of max

	// case 1:
	{
		boundaries := makeBoundaries(10)
		combined := maybeCombineBoundaries(boundaries, 10)
		require.Equal(t, len(boundaries), len(combined))
		require.Equal(t, boundaries, combined)
	}

	{ // test case where boundary length does divide evenly
		boundaries := makeBoundaries(10)
		combined := maybeCombineBoundaries(boundaries, 9)
		require.Equal(t, 5, len(combined))
		require.Equal(t, boundaries[0].Combine(boundaries[1]), combined[0])
		require.Equal(t, boundaries[8].Combine(boundaries[9]), combined[4])

	}

	{ // test case where the original boundary length does not divide evenly
		boundaries := makeBoundaries(33)
		combined := maybeCombineBoundaries(boundaries, 10)
		require.Equal(t, 9, len(combined))
		require.Equal(t, boundaries[0].Combine(boundaries[3]), combined[0])
		require.Equal(t, boundaries[32], combined[8])

	}

}
