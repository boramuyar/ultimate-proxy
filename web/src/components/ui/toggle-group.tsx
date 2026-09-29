import * as React from "react";
import * as ToggleGroupPrimitive from "@radix-ui/react-toggle-group";
import { cn } from "@/lib/utils";

function ToggleGroup({ className, ...props }: React.ComponentProps<typeof ToggleGroupPrimitive.Root>) {
  return <ToggleGroupPrimitive.Root data-slot="toggle-group" className={cn("inline-flex border border-strong bg-card", className)} {...props} />;
}

function ToggleGroupItem({ className, ...props }: React.ComponentProps<typeof ToggleGroupPrimitive.Item>) {
  return (
    <ToggleGroupPrimitive.Item
      data-slot="toggle-group-item"
      className={cn(
        "h-8 cursor-pointer border-l border-strong px-3 font-mono text-[11px] font-bold uppercase tracking-wider text-muted-foreground outline-none first:border-l-0 hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring data-[state=on]:bg-foreground data-[state=on]:text-background data-[state=on]:border-l-muted-foreground",
        className,
      )}
      {...props}
    />
  );
}

export { ToggleGroup, ToggleGroupItem };
