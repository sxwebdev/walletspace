import { cn } from "cn"

// The full address on every pre-signature review, never shortened: comparing
// the whole string is what catches an address-poisoning lookalike that
// shares its first and last characters with the real one.
export function GroupedAddress({ value, className }: { value: string; className?: string }) {
  return <span className={cn("font-mono text-xs leading-relaxed break-all", className)}>{value}</span>
}
