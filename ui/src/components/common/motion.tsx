import * as React from "react"
import { animate, motion, useMotionValue, useTransform } from "motion/react"

export const easeOut = [0.22, 1, 0.36, 1] as const

export function FadeIn({
  children,
  delay = 0,
  className,
  y = 6,
}: {
  children: React.ReactNode
  delay?: number
  className?: string
  y?: number
}) {
  return (
    <motion.div
      className={className}
      initial={{ opacity: 0, y }}
      animate={{ opacity: 1, y: 0 }}
      transition={{ duration: 0.35, ease: easeOut, delay }}
    >
      {children}
    </motion.div>
  )
}

export const listItem = {
  hidden: { opacity: 0, y: 4 },
  show: (i: number) => ({
    opacity: 1,
    y: 0,
    transition: { duration: 0.3, ease: easeOut, delay: Math.min(i, 12) * 0.03 },
  }),
}

// Counts up to a number; the text is formatted by the caller.
export function AnimatedNumber({
  value,
  format,
  className,
}: {
  value: number
  format: (n: number) => string
  className?: string
}) {
  const motionValue = useMotionValue(value)
  const text = useTransform(motionValue, (n) => format(n))
  React.useEffect(() => {
    const controls = animate(motionValue, value, {
      duration: 0.6,
      ease: easeOut,
    })
    return () => controls.stop()
  }, [motionValue, value])
  return <motion.span className={className}>{text}</motion.span>
}
