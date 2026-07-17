import { expect, test } from '../../core/fixtures/base.fixture'
import type { BifrostFixtures } from '../../core/fixtures/base.fixture'

// MCP Tool Groups routes to @enterprise components not present in OSS.
// Tests only verify URL routing; do not add UI assertions for enterprise-only content.
test.describe('MCP Tool Groups', () => {
  test.beforeEach(async ({ mcpToolGroupsPage }: BifrostFixtures) => {
      await mcpToolGroupsPage.goto()
    })

  test('should load MCP tool groups page', async ({ mcpToolGroupsPage }: BifrostFixtures) => {
      await expect(mcpToolGroupsPage.page).toHaveURL(/mcp-tool-groups/)
    })
})
