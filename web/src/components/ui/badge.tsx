import * as React from "react";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "@/lib/utils";

const badgeVariants = cva(
  "inline-flex h-5 items-center gap-1 whitespace-nowrap rounded-full px-2 text-xs font-medium [&>svg]:size-3",
  {
    variants: {
      variant: {
        default: "bg-foreground text-background",
        outline: "border border-input text-foreground",
        muted: "bg-secondary text-muted-foreground",
        good: "bg-good-bg text-good",
        warning: "bg-warning-bg text-warning",
        critical: "bg-critical-bg text-critical",
        info: "bg-info-bg text-info",
      },
    },
    defaultVariants: { variant: "default" },
  },
);

function Badge({ className, variant, ...props }: React.ComponentProps<"span"> & VariantProps<typeof badgeVariants>) {
  return <span data-slot="badge" className={cn(badgeVariants({ variant }), className)} {...props} />;
}

export { Badge, badgeVariants };
