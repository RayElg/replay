import type { Metadata } from 'next'
import './globals.css'

export const metadata: Metadata = {
  title: 'Replay',
  description: 'Agent-first test execution platform',
}

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  )
}
