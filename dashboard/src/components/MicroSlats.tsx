import './MicroSlats.css';

export function MicroSlats({ children, className }: {
  children?: React.ReactNode;
  className?: string;
}) {
  return <div className={`micro-slats ${className || ''}`}>{children}</div>;
}
