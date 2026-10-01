import { Monitor, MoonStar, Sun, type LucideIcon } from 'lucide-react'
import { useTheme, type ThemePreference } from '@/hooks/use-theme'
import { cn } from '@/lib/utils'

interface ThemeOption {
  value: ThemePreference
  icon: LucideIcon
  label: string
  /** Saturated, with a soft glow. */
  activeClassName: string
  /** A muted hint of the same hue. */
  idleClassName: string
}

const OPTIONS: ThemeOption[] = [
  {
    value: 'system',
    icon: Monitor,
    label: 'System',
    activeClassName: 'text-teal-500 drop-shadow-[0_0_6px_rgba(20,184,166,0.45)]',
    idleClassName: 'text-teal-500/60',
  },
  {
    value: 'light',
    icon: Sun,
    label: 'Light',
    activeClassName: 'text-amber-500 drop-shadow-[0_0_7px_rgba(245,158,11,0.6)]',
    idleClassName: 'text-amber-500/60',
  },
  {
    value: 'dark',
    icon: MoonStar,
    label: 'Dark',
    activeClassName: 'text-indigo-400 drop-shadow-[0_0_6px_rgba(129,140,248,0.55)]',
    idleClassName: 'text-indigo-400/60',
  },
]

/** The top bar's three-way theme switch: follow the system, light, or dark. */
export function ThemeToggle() {
  const { preference, setTheme } = useTheme()

  return (
    <div
      role="group"
      aria-label="Theme"
      className="flex h-9 shrink-0 items-center gap-0.5 rounded-full border border-border bg-muted/50 px-1"
    >
      {OPTIONS.map(({ value, icon: Icon, label, activeClassName, idleClassName }) => {
        const isActive = preference === value
        return (
          <button
            key={value}
            type="button"
            title={label}
            aria-label={label}
            aria-pressed={isActive}
            onClick={(e) => {
              // The reveal circles out from the button's centre, which holds
              // for keyboard activation too (pointer coordinates wouldn't).
              const rect = e.currentTarget.getBoundingClientRect()
              setTheme(value, {
                x: rect.left + rect.width / 2,
                y: rect.top + rect.height / 2,
              })
            }}
            className={cn(
              'flex items-center justify-center rounded-full p-1.5 outline-none transition-all duration-200 focus-visible:ring-[3px] focus-visible:ring-ring/50',
              isActive ? 'scale-105 bg-background shadow-1' : 'hover:bg-background/60',
            )}
          >
            <Icon
              className={cn(
                'size-4 transition-[color,filter] duration-200',
                isActive ? activeClassName : idleClassName,
              )}
              aria-hidden="true"
            />
          </button>
        )
      })}
    </div>
  )
}
