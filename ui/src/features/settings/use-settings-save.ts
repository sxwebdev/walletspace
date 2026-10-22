import { useMutation, useQueryClient } from "@tanstack/react-query"

import { toast } from "@/components/ui/toast"
import { queryKeys } from "@/features/session/session"
import { APIError, errorMessage } from "@/lib/api/client"
import type { NetworkOverrides, Settings } from "@/lib/api/types"

// Settings and network overrides share one revision on the server; whichever
// is saved, both caches adopt the new one so the next save is not refused.
export function useAdoptRevision() {
  const queryClient = useQueryClient()
  return {
    settings: (settings: Settings) => {
      queryClient.setQueryData(queryKeys.settings, settings)
      queryClient.setQueryData<NetworkOverrides>(queryKeys.overrides, (data) =>
        data ? { ...data, revision: settings.revision } : data
      )
    },
    overrides: (overrides: NetworkOverrides) => {
      queryClient.setQueryData(queryKeys.overrides, overrides)
      queryClient.setQueryData<Settings>(queryKeys.settings, (data) =>
        data ? { ...data, revision: overrides.revision } : data
      )
    },
  }
}

export function settingsErrorMessage(error: unknown) {
  if (error instanceof APIError && error.status === 412) {
    return "The settings changed in another tab. Reload and try again."
  }
  return errorMessage(error)
}

export function useSettingsMutation<T>(fn: () => Promise<T>, onSuccess: (value: T) => void) {
  return useMutation({
    mutationFn: fn,
    onSuccess: (value) => {
      onSuccess(value)
      toast.add({ type: "success", title: "Settings saved" })
    },
  })
}
