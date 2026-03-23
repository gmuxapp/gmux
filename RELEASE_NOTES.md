- **Fixed mobile keyboard word replacement.** iOS autocorrect and Android
  predictive text would concatenate the corrected word after the original
  instead of replacing it. Replacement events are now intercepted and
  translated into the correct terminal backspace + retype sequence.
