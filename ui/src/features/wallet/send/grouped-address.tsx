import { cn } from "cn"

// The full address in groups of four, spaced by CSS only, so a copy still
// yields one unbroken string. Every pre-signature review uses it: comparing
// groups is what catches an address-poisoning lookalike.
export function GroupedAddress({ value, className }: { value: string; className?: string }) {
  const groups = value.match(/.{1,4}/g) ?? []
  return (
    <span className={cn("font-mono text-xs leading-relaxed break-all", className)}>
      {groups.map((group, i) => (
        <span key={i} className="mr-1 inline-block last:mr-0">
          {group}
        </span>
      ))}
    </span>
  )
}
