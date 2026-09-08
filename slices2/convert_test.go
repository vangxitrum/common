package slices2_test

import (
	"strconv"
	"testing"

	"aioz-depin/internal/slices2"

	"github.com/stretchr/testify/require"
)

func TestConvert(t *testing.T) {
	good := []string{"1", "2", "3", "4"}
	out, err := slices2.Convert(good, strconv.Atoi)
	require.NoError(t, err)
	require.Equal(t, []int{1, 2, 3, 4}, out)

	bad := []string{"1", "bad", "asdf", ""}
	out, err = slices2.Convert(bad, strconv.Atoi)
	require.Error(t, err)
	require.Nil(t, out)
}
