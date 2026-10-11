1. First prompt I wanted claude to generate me a rough scaffolding of the project. Including having the tables for the Postgres database covering, accounts, transfers and history. This was drafted from my intial plan in another chat.

what it did:

All amounts are represented through a whole number equaling the number of cents. Storing whole cents avoids floating-point numbers, which can't represent most decimal fractions exactly. Integer arithmetic on cents is always exact.

problems: 

currently in the scope of what I have to do, i think this is fine but for future purposes, if I include other currencies like japanese yen, they do not have decimals. Or some other currencies have 3 decimal places. In this case I would probably have a hashmap with all currencies I want to support and their respective decimal places and then for every operation invloving these decimals I would replace it with a global variable that sets it to the currency of the user. Then for transfers I would reject transfers between different currencies until I can get the proper exchange values down (through a live currency api?)

another problem is that I can only add and subtract which is what the assigment wants but if i implement anything like intrest where money grows mulitplicatively then there would be wierd rounding issues that ill need to cover. Or like splitting 10 dollars 3 ways, who would get the remaining cent etc. 

a problem that exists in the scope of what I have to do though is a float returning. I realized that Go stores numbers in binary and something like 0.29 doesnt have a binary representation so it will be stored as 0.28999... and when you mutiply by 100 to convert to cents to use, it would be 28.999 which then takes the floor to 28 cents which loses 1 cent.

 A risk that is in scope is a float sneaking back into the code. When I asked Claude about the cents approach, it flagged that Go stores floating-point numbers in binary, and a value like 0.29 has no exact binary representation, so it gets stored as 0.28999999999999998. Multiplying by 100 to convert to cents gives 28.999999999999996, and converting that to an integer truncates it to 28 cents, so 1 cent disappears with no error. I asked it to prove this, and it ran a small Go program showing 0.29, 1.15 and 4.35 all losing a cent, while round amounts like 10.00 and 5.50 came out fine. That's what makes the bug dangerous: tests that use round numbers would pass.

 currently this should problem should be fine since I do not have mutiplication or division.


2. next few prompts to implemnt the features of having an account.