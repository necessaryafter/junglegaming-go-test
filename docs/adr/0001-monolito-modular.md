# ADR 0001 — Monólito modular

## Decisão

Manter o código da aplicação em `internal`, com `app` compondo os módulos,
`wagering` concentrando o negócio financeiro e `platform` fornecendo infraestrutura.
Carteira, transações, ledger, inbox e outbox pertencem ao mesmo módulo porque
compartilham uma transação SQL. Cada módulo expõe sua composição por `module.go`.

O domínio não depende de Fx, HTTP, AWS ou SQL. A interface `Repository` fica em
`application`, que a consome; `postgres` a implementa com pgx e SQL explícito.
A plataforma não importa módulos de negócio. `TestModuleBoundaries` verifica essas fronteiras.

## Motivo

Essa divisão mantém as regras financeiras independentes dos transportes e da infraestrutura.
HTTP e SQS reutilizam o mesmo caso de uso, enquanto carteira e ledger permanecem
na mesma unidade de trabalho. `internal` restringe imports por outros projetos.
