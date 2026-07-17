import { expect, test } from '../../core/fixtures/base.fixture'
import type { BifrostFixtures } from '../../core/fixtures/base.fixture'

// MCP Auth Config routes to @enterprise components not present in OSS.
// Tests only verify URL routing; do not add UI assertions for enterprise-only content.
test.describe('MCP Auth Config', () => {
  test.beforeEach(async ({ mcpAuthConfigPage }: BifrostFixtures) => {
      await mcpAuthConfigPage.goto()
    })

  test('should load MCP auth config page', async ({ mcpAuthConfigPage }: BifrostFixtures) => {
      await expect(mcpAuthConfigPage.page).toHaveURL(/mcp-auth-config/)
    })
})
