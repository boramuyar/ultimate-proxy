import * as React from "react";
import * as LabelPrimitive from "@radix-ui/react-label";
import { cn } from "@/lib/utils";

function Label({ className, ...props }: React.ComponentProps<typeof LabelPrimitive.Root>) {
  return (
    <LabelPrimitive.Root
      data-slot="label"
      className={cn("flex select-none items-center gap-2 font-mono text-[10.5px] font-bold uppercase tracking-[0.12em] text-muted-foreground", className)}
      {...props}
    />
  );
}

export { Label };
