import * as React from "react"
import { cn } from "cn"

import {
  Combobox,
  ComboboxCollection,
  ComboboxContent,
  ComboboxEmpty,
  ComboboxGroup,
  ComboboxInput,
  ComboboxItem,
  ComboboxLabel,
  ComboboxList,
  ComboboxTrigger,
  ComboboxValue,
} from "@/components/ui/combobox"
import type { Network } from "@/lib/api/types"

type Option = { id: string; label: string; testnet: boolean }
type Group = { label: string; items: Option[] }

// A network dropdown with a search field at the top of its popup: there are
// close to twenty networks, and typing "arb" beats scrolling for Arbitrum.
// It looks like the plain Select it replaces — a button showing the current
// network — and searches by network name.
export function NetworkPicker({
  networks,
  value,
  onChange,
  allLabel,
  size = "default",
  align = "start",
  className,
  id,
}: {
  networks: Network[]
  value: string
  onChange: (id: string) => void
  // When set, an extra first option with value "all" (e.g. "All networks").
  allLabel?: string
  size?: "sm" | "default"
  // "end" for a picker on the right edge, so the popup opens inward.
  align?: "start" | "end"
  className?: string
  id?: string
}) {
  const groups = React.useMemo<Group[]>(() => {
    const option = (n: Network): Option => ({
      id: n.id,
      label: n.name,
      testnet: n.testnet,
    })
    const mainnets = networks.filter((n) => !n.testnet).map(option)
    const testnets = networks.filter((n) => n.testnet).map(option)
    return [
      ...(allLabel
        ? [
            {
              label: "",
              items: [{ id: "all", label: allLabel, testnet: false }],
            },
          ]
        : []),
      ...(mainnets.length ? [{ label: "Mainnets", items: mainnets }] : []),
      ...(testnets.length ? [{ label: "Testnets", items: testnets }] : []),
    ]
  }, [networks, allLabel])

  const selected =
    groups.flatMap((g) => g.items).find((o) => o.id === value) ?? null

  return (
    <Combobox
      items={groups}
      value={selected}
      onValueChange={(option: Option | null) => option && onChange(option.id)}
      itemToStringLabel={(option: Option) => option.label}
      isItemEqualToValue={(a: Option, b: Option) => a.id === b.id}
    >
      <ComboboxTrigger
        id={id}
        data-size={size}
        className={cn(
          "flex w-fit items-center justify-between gap-1.5 rounded-3xl border border-transparent bg-input/50 px-3 py-2 text-sm whitespace-nowrap transition-[color,box-shadow,background-color] outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/30 disabled:cursor-not-allowed disabled:opacity-50 data-[size=default]:h-9 data-[size=sm]:h-8",
          className
        )}
      >
        <span className="truncate">
          <ComboboxValue placeholder="Choose a network" />
        </span>
      </ComboboxTrigger>
      <ComboboxContent align={align} className="w-max min-w-(--anchor-width)">
        <ComboboxInput showTrigger={false} placeholder="Search networks" />
        <ComboboxEmpty>No networks found.</ComboboxEmpty>
        <ComboboxList>
          {(group: Group) => (
            <ComboboxGroup key={group.label || "all"} items={group.items}>
              {group.label && <ComboboxLabel>{group.label}</ComboboxLabel>}
              <ComboboxCollection>
                {(option: Option) => (
                  <ComboboxItem key={option.id} value={option}>
                    {option.label}
                  </ComboboxItem>
                )}
              </ComboboxCollection>
            </ComboboxGroup>
          )}
        </ComboboxList>
      </ComboboxContent>
    </Combobox>
  )
}
