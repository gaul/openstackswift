package tests

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestListContainersEmptyDatabase(t *testing.T) {
	c, cleanup := setup()
	defer cleanup()

	ctx := context.Background()
	err := c.Authenticate(ctx)
	assert.NoError(t, err)

	//

	containers, err := c.Containers(ctx, nil)
	assert.NoError(t, err)
	assert.Empty(t, containers)
}
