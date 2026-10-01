import { z } from 'zod'

export const PASSWORD_MIN_LENGTH = 12
export const PASSWORD_MAX_LENGTH = 128

/** Client-side mirror of the server policy (the server is authoritative). */
export function buildPasswordSchema(username: string) {
  return z
    .object({
      current_password: z.string().min(1, 'Enter your current password'),
      new_password: z
        .string()
        .min(PASSWORD_MIN_LENGTH, `Use at least ${PASSWORD_MIN_LENGTH} characters`)
        .max(PASSWORD_MAX_LENGTH, `Use at most ${PASSWORD_MAX_LENGTH} characters`),
      confirm_password: z.string(),
    })
    .superRefine((values, ctx) => {
      if (
        username &&
        values.new_password.toLowerCase() === username.toLowerCase() &&
        values.new_password.length > 0
      ) {
        ctx.addIssue({
          code: 'custom',
          path: ['new_password'],
          message: 'Password can’t be the same as your username',
        })
      }
      if (values.confirm_password !== values.new_password) {
        ctx.addIssue({
          code: 'custom',
          path: ['confirm_password'],
          message: 'Passwords don’t match',
        })
      }
    })
}
