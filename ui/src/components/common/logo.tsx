import { cn } from "cn"

export function Logo({ className }: { className?: string }) {
  return (
    <div
      className={cn(
        "flex size-10 items-center justify-center rounded-2xl bg-primary text-primary-foreground",
        className
      )}
      aria-hidden
    >
      <svg viewBox="0 0 32 32" className="size-[60%]" fill="none">
        <path
          d="M6 10l3.6 12L16 12l6.4 10L26 10"
          stroke="currentColor"
          strokeWidth="2.6"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      </svg>
    </div>
  )
}
