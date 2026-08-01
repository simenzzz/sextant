import react from '@vitejs/plugin-react'
// defineConfig comes from vitest/config, not vite: it is the same function
// widened to accept the `test` block below. Importing it from vite instead
// type-errors on `test`.
import { defineConfig } from 'vitest/config'

export default defineConfig({
  plugins: [react()],
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
    coverage: {
      provider: 'v8',
      // Generated contract types carry no runtime code worth covering, and
      // main.tsx is the bootstrap. Everything else is fair game.
      include: ['src/lib/**', 'src/state/**', 'src/components/**'],
      // Enforced, not just reported. Branch sits below lines because the
      // defensive branches in the transport layer are hard to reach without
      // a real network.
      thresholds: { lines: 80, functions: 80, statements: 80, branches: 70 },
    },
  },
})
