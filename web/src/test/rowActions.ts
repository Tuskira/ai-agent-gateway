import { screen } from '@testing-library/react'
import type { UserEvent } from '@testing-library/user-event'

/** Open a table row's actions menu and choose one of its items. */
export async function chooseRowAction(user: UserEvent, rowLabel: string, action: string) {
  await user.click(await screen.findByRole('button', { name: `Actions for ${rowLabel}` }))
  await user.click(await screen.findByRole('menuitem', { name: action }))
}
