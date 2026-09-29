import * as React from "react";
import { cn } from "@/lib/utils";

function Input({ className, type, ...props }: React.ComponentProps<"input">) {
  return (
    <input
      type={type}
      data-slot="input"
      className={cn(
        "h-9 w-full min-w-0 rounded-md border border-input bg-card px-3 text-sm shadow-xs outline-none transition-[box-shadow,border-color] placeholder:text-faint hover:border-[#c9c9c9] focus-visible:border-[#8f8f8f] focus-visible:ring-3 focus-visible:ring-black/8 disabled:opacity-50",
        className,
      )}
      {...props}
    />
  );
}

export { Input };
